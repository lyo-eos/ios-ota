package linkcore

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func TestLaunchAgentCanonicalIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	binary := "/Users/test/.local/bin/lyo-nodus-ios-ota"
	profile := "/Users/test/Library/Application Support/Owner & Device/iphone.json"
	data, err := launchAgentContents(binary, profile)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if _, err := plist.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if ServiceName != "Lyo Nodus iOS OTA" || ServiceLabel != "lyo-nodus-ios-ota" || fields["Label"] != ServiceLabel {
		t.Fatalf("noncanonical service identity: %v", fields["Label"])
	}
	if filepath.Base(LaunchAgentPath()) != ServiceLabel+".plist" {
		t.Fatal(LaunchAgentPath())
	}
	if !reflect.DeepEqual(fields["ProgramArguments"], []any{binary, "serve", "--profile", profile}) {
		t.Fatalf("profile or executable changed: %v", fields["ProgramArguments"])
	}
	logs := filepath.Join(filepath.Dir(profile), "logs")
	if fields["StandardOutPath"] != filepath.Join(logs, ServiceLabel+".log") || fields["StandardErrorPath"] != filepath.Join(logs, ServiceLabel+".error.log") {
		t.Fatal("log names must use the canonical slug")
	}
	if fields["RunAtLoad"] != true || fields["KeepAlive"] != true || fields["ProcessType"] != "Interactive" {
		t.Fatal("naming must preserve the daemon's existing scheduling")
	}
	for _, key := range []string{"DisplayName", "AssociatedBundleIdentifiers", "BundleProgram"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("unexpected app wrapper metadata: %s", key)
		}
	}
}

func TestLaunchAgentRejectsRelativePaths(t *testing.T) {
	for _, paths := range [][2]string{{"relative", "/tmp/phone.json"}, {"/tmp/agent", "phone.json"}} {
		if _, err := launchAgentContents(paths[0], paths[1]); errorCode(err) != "launch_agent_invalid" {
			t.Fatalf("relative path accepted: %v", paths)
		}
	}
}

// fakeLaunchctl replaces launchctl with respond and records each call.
func fakeLaunchctl(t *testing.T, respond func(arguments []string) error) *[]string {
	t.Helper()
	original, timeout, interval := launchctl, launchAgentRestartTimeout, launchAgentPollInterval
	t.Cleanup(func() {
		launchctl, launchAgentRestartTimeout, launchAgentPollInterval = original, timeout, interval
	})
	launchAgentPollInterval = time.Millisecond
	var calls []string
	launchctl = func(arguments ...string) ([]byte, error) {
		calls = append(calls, strings.Join(arguments, " "))
		return []byte("Bootstrap failed: 5: Input/output error"), respond(arguments)
	}
	return &calls
}

var errServiceNotFound = errors.New("exit status 113")
var errBootstrapIO = errors.New("exit status 5")

func TestRestartLaunchAgentWaitsForBootoutTeardown(t *testing.T) {
	lingering := 3
	calls := fakeLaunchctl(t, func(arguments []string) error {
		switch arguments[0] {
		case "print":
			if lingering > 0 {
				lingering--
				return nil
			}
			return errServiceNotFound
		case "bootstrap":
			if lingering > 0 {
				return errBootstrapIO
			}
		}
		return nil
	})
	if err := restartLaunchAgent("/agent.plist"); err != nil {
		t.Fatal(err)
	}
	service := launchServiceTarget()
	want := []string{
		"bootout " + service,
		"print " + service, "print " + service, "print " + service, "print " + service,
		"bootstrap " + launchTarget() + " /agent.plist",
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("launchctl calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestRestartLaunchAgentRetriesFailedBootstrap(t *testing.T) {
	failures := 2
	calls := fakeLaunchctl(t, func(arguments []string) error {
		switch arguments[0] {
		case "print":
			return errServiceNotFound
		case "bootstrap":
			if failures > 0 {
				failures--
				return errBootstrapIO
			}
		}
		return nil
	})
	if err := restartLaunchAgent("/agent.plist"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.Join(*calls, "\n"), "bootstrap "); got != 3 {
		t.Fatalf("bootstrap ran %d times, want 3: %v", got, *calls)
	}
}

func TestRestartLaunchAgentGivesUpAtDeadline(t *testing.T) {
	calls := fakeLaunchctl(t, func(arguments []string) error {
		switch arguments[0] {
		case "print":
			return nil // the old job never goes away
		case "bootstrap":
			return errBootstrapIO
		}
		return nil
	})
	launchAgentRestartTimeout = 20 * time.Millisecond
	err := restartLaunchAgent("/agent.plist")
	if errorCode(err) != "launch_agent_failed" || !errors.Is(err, errBootstrapIO) {
		t.Fatalf("got %v, want launch_agent_failed wrapping the bootstrap error", err)
	}
	if last := (*calls)[len(*calls)-1]; !strings.HasPrefix(last, "bootstrap ") {
		t.Fatalf("gave up without trying bootstrap: %v", *calls)
	}
}
