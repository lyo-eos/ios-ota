package linkcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

type Request struct {
	TestRun          *TestRunRequest `json:"test_run,omitempty"`
	Command          string          `json:"command"`
	AppPath          string          `json:"app_path,omitempty"`
	BundleIdentifier string          `json:"bundle_identifier,omitempty"`
}

type Response struct {
	*AppReadback
	ResultPath      string `json:"result_path,omitempty"`
	TestCount       int    `json:"test_count,omitempty"`
	ScreenshotCount int    `json:"screenshot_count,omitempty"`
	ScreenshotPNG   []byte `json:"-"`
	ImageBytes      int    `json:"image_bytes,omitempty"`
	OK              bool   `json:"ok"`
	Final           bool   `json:"final"`
	Event           string `json:"event,omitempty"`
	State           string `json:"state,omitempty"`
	Generation      uint64 `json:"generation,omitempty"`
	SessionLosses   uint64 `json:"session_loss_count,omitempty"`
	InstallCount    uint64 `json:"install_count,omitempty"`
	UninstallCount  uint64 `json:"uninstall_count,omitempty"`
	Percent         int    `json:"percent,omitempty"`
	Status          string `json:"status,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	Error           string `json:"error,omitempty"`
	LastErrorCode   string `json:"last_error_code,omitempty"`
	ObservedAt      string `json:"observed_at,omitempty"`
}

type Snapshot struct {
	State          string
	Generation     uint64
	SessionLosses  uint64
	InstallCount   uint64
	UninstallCount uint64
	LastErrorCode  string
}

// A non-nil readback distinguishes a confirmed absent App from no query.
type AppReadback struct {
	App *AppIdentity `json:"app"`
}

func snapshotResponse(snapshot Snapshot) Response {
	return Response{
		OK:             true,
		Final:          true,
		Event:          "status",
		State:          snapshot.State,
		Generation:     snapshot.Generation,
		SessionLosses:  snapshot.SessionLosses,
		InstallCount:   snapshot.InstallCount,
		UninstallCount: snapshot.UninstallCount,
		LastErrorCode:  snapshot.LastErrorCode,
	}
}

func errorResponse(err error) Response {
	return Response{
		OK:        false,
		Final:     true,
		Event:     "error",
		ErrorCode: errorCode(err),
		Error:     publicError(err),
	}
}

func prepareUnixListener(socketPath string) (net.Listener, error) {
	if _, err := os.Lstat(socketPath); err == nil {
		connection, dialErr := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, coded("ios_ota_already_running", "another iOS OTA process owns the control socket", nil)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, coded("control_socket_failed", "could not remove a stale control socket", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, coded("control_socket_failed", "could not inspect the control socket", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, coded("control_socket_failed", "could not bind the control socket", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, coded("control_socket_failed", "could not protect the control socket", err)
	}
	return listener, nil
}

func readRequest(connection net.Conn) (Request, error) {
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	decoder := json.NewDecoder(io.LimitReader(connection, 1<<20))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, coded("control_request_invalid", "could not decode control request", err)
	}
	_ = connection.SetReadDeadline(time.Time{})
	return request, nil
}

func Call(ctx context.Context, profilePath string, request Request, onResponse func(Response)) error {
	socketPath, err := SocketPath(profilePath)
	if err != nil {
		return err
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return coded("ios_ota_not_running", "iOS OTA does not answer on the profile control socket", err)
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return coded("control_request_failed", "could not send control request", err)
	}
	decoder := json.NewDecoder(io.LimitReader(connection, 1<<20))
	for {
		var response Response
		if err := decoder.Decode(&response); err != nil {
			return coded("control_response_failed", "daemon response ended before a final result", err)
		}
		if response.ImageBytes != 0 || (response.OK && response.Event == "screenshot") {
			if request.Command != "screenshot" || !response.OK || !response.Final || response.Event != "screenshot" {
				return coded("control_response_failed", "unexpected screenshot body", nil)
			}
			data, err := readScreenshotBody(io.MultiReader(decoder.Buffered(), connection), response.ImageBytes)
			if err != nil {
				return err
			}
			response.ScreenshotPNG = data
		}
		if onResponse != nil {
			onResponse(response)
		}
		if response.Final {
			if !response.OK {
				return coded(response.ErrorCode, response.Error, nil)
			}
			return nil
		}
	}
}

// json.Encoder terminates the header with one newline. Decode can read ahead
// into the binary body, so Call joins its buffered bytes with the socket.
func readScreenshotBody(reader io.Reader, size int) ([]byte, error) {
	if size <= 0 || size > maxScreenshotBytes {
		return nil, coded("capture_invalid_image", "screenshot payload has an invalid size", nil)
	}
	var newline [1]byte
	if _, err := io.ReadFull(reader, newline[:]); err != nil || newline[0] != '\n' {
		return nil, coded("control_response_failed", "screenshot header has no newline delimiter", err)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, coded("control_response_failed", "screenshot body is incomplete", err)
	}
	return data, nil
}

type safeEncoder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (e *safeEncoder) Encode(response Response) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(response)
}

func writeOneResponse(writer io.Writer, response Response) error {
	return json.NewEncoder(writer).Encode(response)
}

func validateCommand(request Request) error {
	if request.Command == "run-tests" {
		if request.AppPath != "" || request.BundleIdentifier != "" {
			return coded("control_request_invalid", "run-tests accepts only test_run", nil)
		}
		return validateTestRun(request.TestRun)
	}
	if request.TestRun != nil {
		return coded("control_request_invalid", "test_run is only valid for run-tests", nil)
	}
	switch request.Command {
	case "status", "stop", "screenshot":
		if request.AppPath != "" || request.BundleIdentifier != "" {
			return coded("control_request_invalid", fmt.Sprintf("%s does not accept an operation target", request.Command), nil)
		}
	case "install":
		if request.AppPath == "" || request.BundleIdentifier != "" {
			return coded("control_request_invalid", "install requires app_path", nil)
		}
	case "uninstall", "app-read":
		if request.AppPath != "" || !validBundleIdentifier(request.BundleIdentifier) {
			return coded("control_request_invalid", request.Command+" requires one exact valid bundle_identifier", nil)
		}
	default:
		return coded("control_request_invalid", "unknown command", nil)
	}
	return nil
}
