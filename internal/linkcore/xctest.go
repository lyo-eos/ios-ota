package linkcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/danielpaulus/go-ios/ios/testmanagerd"
)

type TestRunRequest struct {
	CaptureBindAddress  string   `json:"capture_bind_address,omitempty"`
	AppID               string   `json:"app_id"`
	RunnerID            string   `json:"runner_id"`
	TestBundle          string   `json:"test_bundle"`
	Tests               []string `json:"tests"`
	RequiredAttachments []string `json:"required_attachments,omitempty"`
	OutputDirectory     string   `json:"output_directory"`
	TimeoutSeconds      int      `json:"timeout_seconds"`
}

type TestRunResult struct {
	OK              bool                     `json:"ok"`
	Generation      uint64                   `json:"generation"`
	Transport       string                   `json:"transport"`
	StartedAt       time.Time                `json:"started_at"`
	FinishedAt      time.Time                `json:"finished_at"`
	RequestedTests  []string                 `json:"requested_tests"`
	Passed          int                      `json:"passed"`
	ScreenshotCount int                      `json:"screenshot_count"`
	Suites          []testmanagerd.TestSuite `json:"suites"`
	Error           string                   `json:"error,omitempty"`
}

var testSelector = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*/test[A-Za-z0-9_]+$`)
var testBundleName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\.xctest$`)

func validateTestRun(r *TestRunRequest, overlay string) error {
	if r == nil || !validBundleIdentifier(r.AppID) || !validBundleIdentifier(r.RunnerID) || !testBundleName.MatchString(r.TestBundle) {
		return coded("test_request_invalid", "run-tests requires valid app, runner and test bundle identifiers", nil)
	}
	if len(r.Tests) == 0 || len(r.Tests) > 256 || r.TimeoutSeconds < 30 || r.TimeoutSeconds > 1800 {
		return coded("test_request_invalid", "select 1–256 test methods and a timeout of 30–1800 seconds", nil)
	}
	if r.CaptureBindAddress != "" && !validCaptureBindAddress(overlay, r.CaptureBindAddress) {
		return coded("test_request_invalid", "capture bind address must be a local "+overlay+" address", nil)
	}
	seen := make(map[string]bool)
	for _, selector := range r.Tests {
		if !testSelector.MatchString(selector) || seen[selector] {
			return coded("test_request_invalid", "test selectors must be unique Class/testMethod names", nil)
		}
		seen[selector] = true
	}
	if !filepath.IsAbs(r.OutputDirectory) || filepath.Clean(r.OutputDirectory) == string(filepath.Separator) {
		return coded("test_request_invalid", "output must identify a new absolute directory", nil)
	}
	if len(r.RequiredAttachments) > 1024 {
		return coded("test_request_invalid", "too many required attachment names", nil)
	}
	for _, name := range r.RequiredAttachments {
		if name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
			return coded("test_request_invalid", "invalid required attachment name", nil)
		}
	}
	return nil
}

func (s *Session) RunTests(ctx context.Context, request TestRunRequest, log io.Writer, attachments string) ([]testmanagerd.TestSuite, error) {
	device, err := s.captureDevice(ctx)
	if err != nil {
		return nil, err
	}
	for _, service := range []string{"com.apple.dt.testmanagerd.remote", "com.apple.coredevice.appservice", "com.apple.coredevice.openstdiosocket"} {
		if device.Rsd.GetPort(service) == 0 {
			return nil, coded("test_service_unavailable", "the active device does not advertise "+service, nil)
		}
	}
	var environment map[string]any
	if request.CaptureBindAddress != "" {
		reader, closeReader, err := s.openScreenshotReader(ctx)
		if err != nil {
			return nil, err
		}
		defer closeReader()
		bridge, err := startCaptureBridge(ctx, s.overlay, request.CaptureBindAddress, reader)
		if err != nil {
			return nil, err
		}
		defer bridge.Close()
		environment = map[string]any{"LYO_SWIFT_CAPTURE_URL": bridge.URL, "LYO_SWIFT_CAPTURE_TOKEN": bridge.Token}
	}
	listener := testmanagerd.NewTestListener(log, log, attachments)
	suites, err := testmanagerd.RunTestWithConfig(ctx, testmanagerd.TestConfig{
		BundleId: request.AppID, TestRunnerBundleId: request.RunnerID,
		XctestConfigName: request.TestBundle, TestsToRun: request.Tests,
		Device: device, Listener: listener, Env: environment,
	})
	if ctx.Err() != nil {
		return suites, ctx.Err()
	}
	return suites, err
}

// Successful process startup is insufficient: every selected test must finish
// successfully and every required named screenshot must be a complete PNG.
func verifyTestEvidence(result *TestRunResult, request TestRunRequest, attachments string) error {
	finished := make(map[string]bool)
	names := make(map[string]bool)
	for si := range result.Suites {
		for ti := range result.Suites[si].TestCases {
			test := &result.Suites[si].TestCases[ti]
			class := test.ClassName
			if dot := strings.LastIndexByte(class, '.'); dot >= 0 {
				class = class[dot+1:]
			}
			selector := class + "/" + strings.TrimSuffix(test.MethodName, "()")
			if test.Status != testmanagerd.StatusPassed || finished[selector] {
				return fmt.Errorf("test %s did not pass exactly once: %s %s", selector, test.Status, test.Err.Message)
			}
			finished[selector] = true
			result.Passed++
			for ai := range test.Attachments {
				a := &test.Attachments[ai]
				if a.UniformTypeIdentifier != "public.png" {
					continue
				}
				if filepath.Dir(a.Path) != attachments {
					return fmt.Errorf("attachment path is outside the test output")
				}
				info, err := os.Lstat(a.Path)
				if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxScreenshotBytes {
					return fmt.Errorf("attachment %q is missing or has an invalid size", a.Name)
				}
				data, err := os.ReadFile(a.Path)
				if err != nil {
					return err
				}
				config, err := png.DecodeConfig(bytes.NewReader(data))
				if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxScreenshotPixels {
					return fmt.Errorf("attachment %q is not a bounded PNG", a.Name)
				}
				if _, err := png.Decode(bytes.NewReader(data)); err != nil {
					return fmt.Errorf("attachment %q is corrupt: %w", a.Name, err)
				}
				newPath := a.Path + ".png"
				if err := os.Rename(a.Path, newPath); err != nil {
					return err
				}
				a.Path = newPath
				names[a.Name] = true
				result.ScreenshotCount++
			}
		}
	}
	if len(finished) != len(request.Tests) {
		return fmt.Errorf("received %d completed tests for %d selected methods", len(finished), len(request.Tests))
	}
	for _, selector := range request.Tests {
		if !finished[selector] {
			return fmt.Errorf("selected test %s did not finish", selector)
		}
	}
	for _, name := range request.RequiredAttachments {
		if !names[name] {
			return fmt.Errorf("required PNG attachment %q is missing", name)
		}
	}
	return nil
}

func (d *Daemon) runTests(ctx context.Context, request TestRunRequest) (Response, error) {
	if err := validateTestRun(&request, d.config.Overlay); err != nil {
		return Response{}, err
	}
	if !d.operationMu.TryLock() {
		return Response{}, coded("session_busy", "another operation owns the device session", nil)
	}
	defer d.operationMu.Unlock()
	session := d.currentSession()
	if session == nil {
		return Response{}, coded("session_not_active", "no existing RemotePairing session is available", nil)
	}
	if err := os.Mkdir(request.OutputDirectory, 0o700); err != nil {
		return Response{}, coded("test_output_failed", "could not create a new test output directory", err)
	}
	attachments := filepath.Join(request.OutputDirectory, "attachments")
	if err := os.Mkdir(attachments, 0o700); err != nil {
		return Response{}, err
	}
	log, err := os.OpenFile(filepath.Join(request.OutputDirectory, "runner.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Response{}, err
	}
	defer log.Close()
	transport := "ios-ota-tailnet-rsd"
	if d.config.Overlay == OverlayWireGuard {
		transport = "ios-ota-wireguard-rsd"
	}
	result := TestRunResult{Generation: d.Snapshot().Generation, Transport: transport, StartedAt: time.Now().UTC(), RequestedTests: request.Tests}
	testCtx, cancel := context.WithTimeout(ctx, time.Duration(request.TimeoutSeconds)*time.Second)
	defer cancel()
	result.Suites, err = session.RunTests(testCtx, request, log, attachments)
	if testCtx.Err() != nil {
		err = testCtx.Err()
	}
	if err == nil {
		err = verifyTestEvidence(&result, request, attachments)
	}
	result.FinishedAt = time.Now().UTC()
	result.OK = err == nil
	if err != nil {
		result.Error = err.Error()
	}
	resultPath := filepath.Join(request.OutputDirectory, "result.json")
	data, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		return Response{}, marshalErr
	}
	if writeErr := os.WriteFile(resultPath, append(data, '\n'), 0o600); writeErr != nil {
		return Response{}, writeErr
	}
	response := Response{OK: result.OK, Final: true, Event: "tests_finished", State: d.Snapshot().State, Generation: result.Generation,
		ResultPath: resultPath, TestCount: result.Passed, ScreenshotCount: result.ScreenshotCount}
	if err != nil {
		response.ErrorCode = "test_run_failed"
		response.Error = strings.SplitN(result.Error, "\n", 2)[0]
	}
	return response, nil
}
