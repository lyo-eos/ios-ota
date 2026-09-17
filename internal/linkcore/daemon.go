package linkcore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios/testmanagerd"
)

const (
	StateWaiting      = "waiting_for_listener"
	StateConnecting   = "connecting"
	StateActive       = "active"
	StateRecovering   = "recovering"
	StateInstalling   = "installing"
	StateUninstalling = "uninstalling"
	StateLost         = "session_lost"
	StateStopping     = "stopping"
)

type Daemon struct {
	location    *locationSession
	config      Config
	profilePath string
	socketPath  string
	openSession func(context.Context, Config) (sessionHandle, error)
	validateApp func(context.Context, string) (AppBundle, error)

	mu             sync.RWMutex
	operationMu    sync.Mutex
	state          string
	generation     uint64
	sessionLosses  uint64
	installCount   uint64
	uninstallCount uint64
	lastErrorCode  string
	session        sessionHandle
	cancel         context.CancelFunc
}

type sessionHandle interface {
	OpenLocation(context.Context) (locationDriver, error)
	Screenshot(context.Context) ([]byte, error)
	RunTests(context.Context, TestRunRequest, io.Writer, string) ([]testmanagerd.TestSuite, error)
	Close() error
	Done() <-chan error
	Install(context.Context, AppBundle, func(int, string)) error
	Uninstall(context.Context, string) error
}

func NewDaemon(config Config, profilePath string) (*Daemon, error) {
	socketPath, err := SocketPath(profilePath)
	if err != nil {
		return nil, err
	}
	daemon := &Daemon{
		config:      config,
		profilePath: profilePath,
		socketPath:  socketPath,
		state:       StateWaiting,
		openSession: func(ctx context.Context, config Config) (sessionHandle, error) {
			return OpenSession(ctx, config)
		},
		validateApp: ValidateApp,
	}
	daemon.location = newLocationSession(func(ctx context.Context) (locationDriver, error) {
		session := daemon.currentSession()
		if session == nil {
			return nil, errors.New("phone tunnel is not active")
		}
		return session.OpenLocation(ctx)
	})
	return daemon, nil
}

func (d *Daemon) Snapshot() Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return Snapshot{
		State:          d.state,
		Generation:     d.generation,
		SessionLosses:  d.sessionLosses,
		InstallCount:   d.installCount,
		UninstallCount: d.uninstallCount,
		LastErrorCode:  d.lastErrorCode,
	}
}

func (d *Daemon) setState(state string, lastErrorCode string) {
	d.mu.Lock()
	d.state = state
	d.lastErrorCode = lastErrorCode
	d.mu.Unlock()
}

func (d *Daemon) currentSession() sessionHandle {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.session
}

func (d *Daemon) establish(ctx context.Context) {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if d.currentSession() != nil || ctx.Err() != nil {
		return
	}
	d.setState(StateConnecting, "")
	connectCtx, cancel := context.WithTimeout(ctx, d.config.ConnectTimeout())
	session, err := d.openSession(connectCtx, d.config)
	cancel()
	if err != nil {
		code := errorCode(err)
		d.setState(StateWaiting, code)
		if code != "remote_pairing_cold" {
			slog.Warn(ServiceName+" acquisition failed", "errorCode", code)
		}
		return
	}
	d.mu.Lock()
	d.session = session
	d.generation++
	d.state = StateActive
	d.lastErrorCode = ""
	generation := d.generation
	d.mu.Unlock()
	slog.Info(ServiceName+" session active", "generation", generation)
}

func (d *Daemon) loseSession(generation uint64, err error) {
	d.mu.Lock()
	if d.session == nil || d.generation != generation {
		d.mu.Unlock()
		return
	}
	session := d.session
	d.session = nil
	d.state = StateLost
	d.lastErrorCode = "session_lost"
	d.sessionLosses++
	d.mu.Unlock()
	d.location.lost()
	if session != nil {
		_ = session.Close()
	}
	slog.Warn(ServiceName+" outer tunnel stopped", "generation", generation, "error", err)
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (d *Daemon) connectionLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if d.currentSession() == nil {
			d.setState(StateWaiting, d.Snapshot().LastErrorCode)
			d.establish(ctx)
			if d.currentSession() == nil && !waitContext(ctx, d.config.RetryInterval()) {
				return
			}
			continue
		}
		session := d.currentSession()
		generation := d.Snapshot().Generation
		select {
		case <-ctx.Done():
			return
		case err, ok := <-session.Done():
			if ctx.Err() != nil {
				return
			}
			if !ok && err == nil {
				err = errors.New("outer userspace tunnel stopped")
			}
			d.operationMu.Lock()
			d.loseSession(generation, err)
			d.operationMu.Unlock()
		}
	}
}

func (d *Daemon) install(ctx context.Context, appPath string, progress func(int, string)) error {
	validationCtx, validationCancel := context.WithTimeout(ctx, 30*time.Second)
	app, err := d.validateApp(validationCtx, appPath)
	validationCancel()
	if err != nil {
		return err
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	session := d.currentSession()
	if session == nil {
		return coded("session_not_active", "daemon is waiting for a usable RemotePairing listener", nil)
	}
	d.setState(StateInstalling, "")
	installCtx, cancel := context.WithTimeout(ctx, d.config.InstallTimeout())
	err = session.Install(installCtx, app, progress)
	cancel()
	if err != nil {
		d.setState(StateRecovering, "service_unresponsive")
		slog.Warn(ServiceName+" install service failed; retaining outer tunnel", "generation", d.Snapshot().Generation, "errorCode", errorCode(err))
		return err
	}
	d.mu.Lock()
	d.installCount++
	d.state = StateActive
	d.lastErrorCode = ""
	d.mu.Unlock()
	return nil
}

func (d *Daemon) uninstall(ctx context.Context, bundleIdentifier string) error {
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	session := d.currentSession()
	if session == nil {
		return coded("session_not_active", "daemon is waiting for a usable RemotePairing listener", nil)
	}
	d.setState(StateUninstalling, "")
	uninstallCtx, cancel := context.WithTimeout(ctx, d.config.InstallTimeout())
	err := session.Uninstall(uninstallCtx, bundleIdentifier)
	cancel()
	if err != nil {
		d.setState(StateRecovering, "service_unresponsive")
		slog.Warn(ServiceName+" uninstall service failed; retaining outer tunnel", "generation", d.Snapshot().Generation, "errorCode", errorCode(err))
		return err
	}
	d.mu.Lock()
	d.uninstallCount++
	d.state = StateActive
	d.lastErrorCode = ""
	d.mu.Unlock()
	return nil
}

func (d *Daemon) handleConnection(ctx context.Context, connection net.Conn) {
	defer connection.Close()
	request, err := readRequest(connection)
	if err != nil {
		_ = writeOneResponse(connection, errorResponse(err))
		return
	}
	if err := validateCommand(request); err != nil {
		_ = writeOneResponse(connection, errorResponse(err))
		return
	}
	encoder := &safeEncoder{encoder: jsonEncoder(connection)}
	switch request.Command {
	case "run-tests":
		testCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		// The client sends exactly one request. EOF means its owner cancelled or
		// disconnected; cancel this test without tearing down the pairing tunnel.
		go func() { _, _ = io.Copy(io.Discard, connection); cancel() }()
		_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := encoder.Encode(Response{OK: true, Event: "tests_started", State: d.Snapshot().State}); err != nil {
			return
		}
		result, err := d.runTests(testCtx, *request.TestRun)
		if err != nil {
			result = errorResponse(err)
		}
		_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = encoder.Encode(result)
	case "screenshot":
		result, err := d.screenshot(ctx)
		if err != nil {
			_ = encoder.Encode(errorResponse(err))
			return
		}
		_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := encoder.Encode(result); err == nil {
			_, _ = connection.Write(result.ScreenshotPNG)
		}
	case "status":
		_ = encoder.Encode(snapshotResponse(d.Snapshot()))
	case "stop":
		_ = encoder.Encode(Response{OK: true, Final: true, Event: "stopping", State: StateStopping})
		if d.cancel != nil {
			d.cancel()
		}
	case "install":
		_ = encoder.Encode(Response{OK: true, Final: false, Event: "accepted", State: d.Snapshot().State})
		err := d.install(ctx, request.AppPath, func(percent int, status string) {
			_ = encoder.Encode(Response{OK: true, Final: false, Event: "progress", Percent: percent, Status: status})
		})
		if err != nil {
			_ = encoder.Encode(errorResponse(err))
			return
		}
		snapshot := d.Snapshot()
		_ = encoder.Encode(Response{
			OK:           true,
			Final:        true,
			Event:        "installed",
			State:        snapshot.State,
			Generation:   snapshot.Generation,
			InstallCount: snapshot.InstallCount,
		})
	case "uninstall":
		_ = encoder.Encode(Response{OK: true, Final: false, Event: "accepted", State: d.Snapshot().State})
		if err := d.uninstall(ctx, request.BundleIdentifier); err != nil {
			_ = encoder.Encode(errorResponse(err))
			return
		}
		snapshot := d.Snapshot()
		_ = encoder.Encode(Response{
			OK:             true,
			Final:          true,
			Event:          "uninstalled",
			State:          snapshot.State,
			Generation:     snapshot.Generation,
			UninstallCount: snapshot.UninstallCount,
		})
	}
}

func jsonEncoder(connection net.Conn) *json.Encoder {
	return json.NewEncoder(connection)
}

func (d *Daemon) Serve(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	defer cancel()
	if d.config.Location != nil {
		server, err := startLocationHTTPS(ctx, *d.config.Location, d.location, func() bool { return d.currentSession() != nil })
		if err != nil {
			return err
		}
		defer server.Close()
		go d.location.pulse(ctx)
	}
	listener, err := prepareUnixListener(d.socketPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(d.socketPath)
	}()
	go d.connectionLoop(ctx)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	slog.Info(ServiceName + " daemon listening")
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			return coded("control_socket_failed", "control socket accept failed", err)
		}
		go d.handleConnection(ctx, connection)
	}
	d.setState(StateStopping, "")
	if d.config.Location != nil {
		<-d.location.done
	}

	d.operationMu.Lock()
	if session := d.currentSession(); session != nil {
		_ = session.Close()
		d.mu.Lock()
		d.session = nil
		d.mu.Unlock()
	}
	d.operationMu.Unlock()
	return nil
}
