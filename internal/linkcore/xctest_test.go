package linkcore

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios/testmanagerd"
)

func testRunRequest(t *testing.T) TestRunRequest {
	t.Helper()
	return TestRunRequest{AppID: "lyo.swift", RunnerID: "lyo.swift.uitests.xctrunner", TestBundle: "LyoSwiftUITests.xctest",
		Tests: []string{"CaptureTests/testTap"}, OutputDirectory: filepath.Join(t.TempDir(), "run"), TimeoutSeconds: 30}
}

func passedSuite() []testmanagerd.TestSuite {
	return []testmanagerd.TestSuite{{Name: "CaptureTests", TestCases: []testmanagerd.TestCase{{ClassName: "Module.CaptureTests", MethodName: "testTap", Status: testmanagerd.StatusPassed}}}}
}

func TestRunRequiresExactFinishedMethods(t *testing.T) {
	request := testRunRequest(t)
	for _, scenario := range []struct {
		name   string
		suites []testmanagerd.TestSuite
	}{
		{"no tests", nil},
		{"wrong method", []testmanagerd.TestSuite{{TestCases: []testmanagerd.TestCase{{ClassName: "CaptureTests", MethodName: "testWrong", Status: testmanagerd.StatusPassed}}}}},
		{"failed", []testmanagerd.TestSuite{{TestCases: []testmanagerd.TestCase{{ClassName: "CaptureTests", MethodName: "testTap", Status: testmanagerd.StatusFailed}}}}},
		{"duplicate", append(passedSuite(), passedSuite()...)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := TestRunResult{Suites: scenario.suites}
			if err := verifyTestEvidence(&result, request, t.TempDir()); err == nil {
				t.Fatal("incomplete or mismatched execution passed")
			}
		})
	}
}

func TestRunRequiresValidNamedPNG(t *testing.T) {
	request := testRunRequest(t)
	request.RequiredAttachments = []string{"after-tap"}
	directory := t.TempDir()
	result := TestRunResult{Suites: passedSuite()}
	if err := verifyTestEvidence(&result, request, directory); err == nil {
		t.Fatal("missing screenshot passed")
	}
	path := filepath.Join(directory, "attachment")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	file.Close()
	result = TestRunResult{Suites: passedSuite()}
	result.Suites[0].TestCases[0].Attachments = []testmanagerd.TestAttachment{{Name: "after-tap", Path: path, UniformTypeIdentifier: "public.png"}}
	if err := verifyTestEvidence(&result, request, directory); err != nil {
		t.Fatal(err)
	}
	if result.Passed != 1 || result.ScreenshotCount != 1 {
		t.Fatal(result)
	}
	if _, err := os.Stat(path + ".png"); err != nil {
		t.Fatal(err)
	}
}

func TestRunFailureKeepsTunnelAndWritesResult(t *testing.T) {
	daemon := testDaemon(t)
	session := &fakeSession{runTests: func(context.Context, TestRunRequest, io.Writer, string) ([]testmanagerd.TestSuite, error) {
		return nil, errors.New("native runner did not start")
	}}
	daemon.session = session
	daemon.state = StateActive
	daemon.generation = 7
	response, err := daemon.runTests(context.Background(), testRunRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if response.OK || response.ErrorCode != "test_run_failed" || response.Generation != 7 {
		t.Fatal(response)
	}
	if session.closed.Load() != 0 || daemon.Snapshot().State != StateActive {
		t.Fatal("test error closed or changed deployment session")
	}
	data, err := os.ReadFile(response.ResultPath)
	if err != nil {
		t.Fatal(err)
	}
	var result TestRunResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error == "" {
		t.Fatal("failure not recorded")
	}
}

func TestRunCancellationCannotPass(t *testing.T) {
	daemon := testDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	daemon.session = &fakeSession{runTests: func(context.Context, TestRunRequest, io.Writer, string) ([]testmanagerd.TestSuite, error) {
		cancel()
		return passedSuite(), nil
	}}
	response, err := daemon.runTests(ctx, testRunRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if response.OK {
		t.Fatal("cancelled execution reported success")
	}
}

func TestRunRejectsBusyAndExistingOutput(t *testing.T) {
	daemon := testDaemon(t)
	daemon.session = &fakeSession{}
	request := testRunRequest(t)
	daemon.operationMu.Lock()
	_, err := daemon.runTests(context.Background(), request)
	daemon.operationMu.Unlock()
	if errorCode(err) != "session_busy" {
		t.Fatal(err)
	}
	if err := os.Mkdir(request.OutputDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = daemon.runTests(context.Background(), request)
	if errorCode(err) != "test_output_failed" {
		t.Fatal(err)
	}
}

func TestRunControlValidation(t *testing.T) {
	request := testRunRequest(t)
	if err := validateCommand(Request{Command: "run-tests", TestRun: &request}, OverlayTailscale); err != nil {
		t.Fatal(err)
	}
	for _, command := range []Request{{Command: "run-tests"}, {Command: "status", TestRun: &request}, {Command: "run-tests", TestRun: &request, AppPath: "app"}} {
		if err := validateCommand(command, OverlayTailscale); err == nil {
			t.Fatal("invalid mixed command accepted")
		}
	}
	request.Tests = append(request.Tests, request.Tests[0])
	if err := validateTestRun(&request, OverlayTailscale); err == nil {
		t.Fatal("duplicate tests accepted")
	}
}

func TestRunClientDisconnectCancelsOnlyInnerOperation(t *testing.T) {
	daemon := testDaemon(t)
	started, cancelled := make(chan struct{}), make(chan struct{})
	session := &fakeSession{runTests: func(ctx context.Context, _ TestRunRequest, _ io.Writer, _ string) ([]testmanagerd.TestSuite, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	daemon.session = session
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() { daemon.handleConnection(context.Background(), server); close(done) }()
	request := testRunRequest(t)
	if err := json.NewEncoder(client).Encode(Request{Command: "run-tests", TestRun: &request}); err != nil {
		t.Fatal(err)
	}
	var accepted Response
	if err := json.NewDecoder(client).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Event != "tests_started" {
		t.Fatal(accepted)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test did not start")
	}
	client.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect left test running")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish")
	}
	if session.closed.Load() != 0 {
		t.Fatal("client disconnect closed outer tunnel")
	}
}
