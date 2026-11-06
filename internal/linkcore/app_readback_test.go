package linkcore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
)

func TestReadbackRequiresExactInstalledIdentity(t *testing.T) {
	expected := AppIdentity{BundleIdentifier: "lyo.nodus", Version: "2.70.0", Build: "256"}
	for _, test := range []struct {
		name         string
		version      any
		build        any
		duplicate    bool
		readError    bool
		installError bool
	}{
		{"exact", "2.70.0", "256", false, false, false},
		{"old-build", "2.70.0", "251", false, false, true},
		{"old-version", "2.69.4", "256", false, false, true},
		{"missing-version", nil, "256", false, true, true},
		{"numeric-build", "2.70.0", 256, false, true, true},
		{"empty-build", "2.70.0", "", false, true, true},
		{"control-character", "2.70.0\n", "256", false, true, true},
		{"oversized-version", strings.Repeat("1", 129), "256", false, true, true},
		{"duplicate", "2.70.0", "256", true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := installationproxy.AppInfo{"CFBundleIdentifier": "lyo.nodus", "CFBundleShortVersionString": test.version, "CFBundleVersion": test.build, "PrivateProperty": "must-not-escape"}
			rows := []installationproxy.AppInfo{{"CFBundleIdentifier": "unrelated.application"}, row}
			if test.duplicate {
				rows = append(rows, row)
			}
			actual, err := appFromReadback(rows, expected.BundleIdentifier)
			if (err != nil) != test.readError {
				t.Fatalf("read error = %v", err)
			}
			if err := verifyInstalledApp(expected, actual); (err != nil) != test.installError {
				t.Fatalf("install error = %v", err)
			}
			encoded, err := json.Marshal(actual)
			if err != nil || strings.Contains(string(encoded), "must-not-escape") || strings.Contains(string(encoded), "unrelated") {
				t.Fatalf("unbounded readback: %s, %v", encoded, err)
			}
		})
	}
	absent, err := appFromReadback(nil, expected.BundleIdentifier)
	if err != nil || absent != nil || verifyInstalledApp(expected, absent) == nil {
		t.Fatalf("absent App was accepted: %#v %v", absent, err)
	}
	if _, err := appFromReadback([]installationproxy.AppInfo{{"CFBundleIdentifier": 42}}, expected.BundleIdentifier); errorCode(err) != "app_read_invalid" {
		t.Fatalf("malformed inventory was reported as absence: %v", err)
	}
}

// Exercise the real pinned plist/shim connection, including cancellation during
// service startup; a caller deadline must not leave the native query blocked.
func TestAppReadUsesBoundedExistingInstallationProxy(t *testing.T) {
	for _, phase := range []string{"complete", "checkin", "browse"} {
		t.Run(phase, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			finished := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					finished <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				var envelope [20]byte
				if _, err := io.ReadFull(conn, envelope[:]); err != nil {
					finished <- err
					return
				}
				if !net.IP(envelope[:16]).Equal(net.ParseIP("fd00::42")) || binary.LittleEndian.Uint32(envelope[16:]) != 62001 {
					finished <- errors.New("query changed paired endpoint")
					return
				}
				codec := ios.NewPlistCodecReadWriter(conn, conn)
				var checkin map[string]any
				if err := codec.Read(&checkin); err != nil {
					finished <- err
					return
				}
				if checkin["Request"] != "RSDCheckin" {
					finished <- errors.New("missing shim checkin")
					return
				}
				if phase == "checkin" {
					finished <- waitDiscoveryClose(conn)
					return
				}
				if err := codec.Write(map[string]any{"Request": "RSDCheckin"}); err != nil {
					finished <- err
					return
				}
				if err := codec.Write(map[string]any{"Request": "StartService"}); err != nil {
					finished <- err
					return
				}
				var request map[string]any
				if err := codec.Read(&request); err != nil {
					finished <- err
					return
				}
				if request["Command"] != "Browse" {
					finished <- errors.New("query attempted a mutation")
					return
				}
				if phase == "browse" {
					finished <- waitDiscoveryClose(conn)
					return
				}
				err = codec.Write(map[string]any{"Status": "Complete", "CurrentIndex": uint64(0), "CurrentAmount": uint64(1), "CurrentList": []map[string]any{{"CFBundleIdentifier": "lyo.nodus", "CFBundleShortVersionString": "2.70.0", "CFBundleVersion": "256"}}})
				finished <- err
			}()
			device := ios.DeviceEntry{Address: "fd00::42", UserspaceTUN: true, UserspaceTUNHost: "127.0.0.1", UserspaceTUNPort: listener.Addr().(*net.TCPAddr).Port,
				Rsd: ios.RsdHandshakeResponse{Services: map[string]ios.RsdServiceEntry{"com.apple.mobile.installation_proxy.shim.remote": {Port: 62001}}}}
			session := &Session{device: device}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			app, err := session.ReadApp(ctx, "lyo.nodus")
			if phase == "complete" {
				if err != nil || app == nil || app.Build != "256" || app.Version != "2.70.0" {
					t.Errorf("native readback = %#v, %v", app, err)
				}
			} else if errorCode(err) != "app_read_failed" || app != nil {
				t.Errorf("failed native query became absence: %#v, %v", app, err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDaemonDoesNotCountOldBuildAsInstalled(t *testing.T) {
	daemon := testDaemon(t)
	expected := AppIdentity{BundleIdentifier: "lyo.nodus", Version: "2.70.0", Build: "256"}
	old := AppIdentity{BundleIdentifier: "lyo.nodus", Version: "2.69.4", Build: "251"}
	daemon.validateApp = func(context.Context, string) (AppBundle, error) {
		return AppBundle{Path: "/private/Test.app", AppIdentity: expected}, nil
	}
	daemon.session = &fakeSession{installedApp: &old}
	daemon.state = StateActive
	app, err := daemon.install(context.Background(), "/private/Test.app", func(int, string) {})
	if errorCode(err) != "install_failed" || app != nil || daemon.Snapshot().InstallCount != 0 {
		t.Fatalf("false install success: %#v %v %#v", app, err, daemon.Snapshot())
	}
	if daemon.Snapshot().State != StateRecovering || daemon.currentSession() == nil {
		t.Fatal("mismatch must retain the existing outer tunnel for reconciliation")
	}
}

func TestAppReadCannotEnterDuringAnAcceptedInstall(t *testing.T) {
	daemon := testDaemon(t)
	identity := AppIdentity{BundleIdentifier: "lyo.nodus", Version: "2.70.0", Build: "256"}
	daemon.validateApp = func(context.Context, string) (AppBundle, error) {
		return AppBundle{Path: "/private/Test.app", AppIdentity: identity}, nil
	}
	entered, release := make(chan struct{}), make(chan struct{})
	daemon.session = &fakeSession{
		install: func(context.Context, AppBundle, func(int, string)) error { close(entered); <-release; return nil },
		readApp: func(context.Context, string) (*AppIdentity, error) { return &identity, nil },
	}
	completed := make(chan error, 1)
	go func() {
		_, err := daemon.install(context.Background(), "/private/Test.app", func(int, string) {})
		completed <- err
	}()
	<-entered
	_, err := daemon.readApp(context.Background(), identity.BundleIdentifier)
	close(release)
	if installErr := <-completed; installErr != nil {
		t.Fatal(installErr)
	}
	if errorCode(err) != "operation_busy" {
		t.Fatalf("read entered an active install: %v", err)
	}
	app, err := daemon.readApp(context.Background(), identity.BundleIdentifier)
	if err != nil || app == nil || *app != identity {
		t.Fatalf("read after install: %#v %v", app, err)
	}
	if daemon.Snapshot().InstallCount != 1 || daemon.Snapshot().UninstallCount != 0 {
		t.Fatal("query changed operation counters")
	}
}

func readAppWire(t *testing.T, daemon *Daemon) map[string]any {
	t.Helper()
	server, client := net.Pipe()
	finished := make(chan struct{})
	go func() { daemon.handleConnection(context.Background(), server); close(finished) }()
	t.Cleanup(func() { client.Close(); <-finished })
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(client).Encode(Request{Command: "app-read", BundleIdentifier: "lyo.nodus"}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.NewDecoder(client).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAppReadWireDistinguishesAbsentPresentAndFailure(t *testing.T) {
	for _, state := range []string{"absent", "present", "failed"} {
		t.Run(state, func(t *testing.T) {
			daemon := testDaemon(t)
			daemon.session = &fakeSession{readApp: func(ctx context.Context, bundle string) (*AppIdentity, error) {
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > 30*time.Second || bundle != "lyo.nodus" {
					t.Error("query lost its bound or target")
				}
				if state == "failed" {
					return nil, coded("app_read_failed", "phone service failed", errors.New("private detail"))
				}
				if state == "absent" {
					return nil, nil
				}
				return &AppIdentity{BundleIdentifier: bundle, Version: "2.70.0", Build: "256"}, nil
			}}
			result := readAppWire(t, daemon)
			app, queried := result["app"]
			if state == "failed" {
				if queried || result["ok"] != false || result["error_code"] != "app_read_failed" {
					t.Fatalf("failure looked like absence: %#v", result)
				}
				return
			}
			if !queried || result["ok"] != true || result["event"] != "app_read" {
				t.Fatalf("missing readback: %#v", result)
			}
			if _, err := time.Parse(time.RFC3339Nano, result["observed_at"].(string)); err != nil {
				t.Fatal(err)
			}
			if state == "absent" && app != nil {
				t.Fatalf("unexpected App: %#v", app)
			}
			if state == "present" {
				metadata, ok := app.(map[string]any)
				if !ok || len(metadata) != 3 || metadata["build"] != "256" || metadata["version"] != "2.70.0" || metadata["bundle_identifier"] != "lyo.nodus" {
					t.Fatalf("invalid App readback: %#v", app)
				}
			}
		})
	}
}
