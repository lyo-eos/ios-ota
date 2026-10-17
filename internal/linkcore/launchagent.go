package linkcore

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const ServiceName = "Lyo Nodus iOS OTA"
const ServiceLabel = "lyo-nodus-ios-ota"

// launchctl runs /bin/launchctl and returns its combined output; tests
// replace it.
var launchctl = func(arguments ...string) ([]byte, error) {
	return exec.Command("/bin/launchctl", arguments...).CombinedOutput()
}

var (
	launchAgentRestartTimeout = 10 * time.Second
	launchAgentPollInterval   = 100 * time.Millisecond
)

func LaunchAgentPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", ServiceLabel+".plist")
}

func xmlEscape(value string) string {
	var buffer bytes.Buffer
	_ = xml.EscapeText(&buffer, []byte(value))
	return buffer.String()
}

func launchTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchServiceTarget() string {
	return launchTarget() + "/" + ServiceLabel
}

func launchAgentContents(binaryPath string, profilePath string) ([]byte, error) {
	if !filepath.IsAbs(binaryPath) || !filepath.IsAbs(profilePath) {
		return nil, coded("launch_agent_invalid", "binary and profile paths must be absolute", nil)
	}
	logsDirectory := filepath.Join(filepath.Dir(profilePath), "logs")
	contents := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
    <string>--profile</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, ServiceLabel, xmlEscape(binaryPath), xmlEscape(profilePath),
		xmlEscape(filepath.Join(logsDirectory, ServiceLabel+".log")),
		xmlEscape(filepath.Join(logsDirectory, ServiceLabel+".error.log")))
	return []byte(contents), nil
}

func InstallLaunchAgent(binaryPath string, profilePath string) error {
	contents, err := launchAgentContents(binaryPath, profilePath)
	if err != nil {
		return err
	}
	logsDirectory := filepath.Join(filepath.Dir(profilePath), "logs")
	if err := os.MkdirAll(logsDirectory, 0o700); err != nil {
		return coded("launch_agent_failed", "could not create log directory", err)
	}
	path := LaunchAgentPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return coded("launch_agent_failed", "could not create LaunchAgents directory", err)
	}
	if err := writeAtomic(path, contents, 0o644); err != nil {
		return coded("launch_agent_failed", "could not write LaunchAgent", err)
	}
	return restartLaunchAgent(path)
}

// restartLaunchAgent replaces the loaded job with the plist at path. bootout
// returns before launchd has torn the old job down, and bootstrapping the same
// label in that window fails with an I/O error, so wait for the old job to go
// and retry bootstrap until the deadline.
func restartLaunchAgent(path string) error {
	_, _ = launchctl("bootout", launchServiceTarget())
	deadline := time.Now().Add(launchAgentRestartTimeout)
	for launchAgentLoaded() && time.Now().Before(deadline) {
		time.Sleep(launchAgentPollInterval)
	}
	for {
		output, err := launchctl("bootstrap", launchTarget(), path)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return coded("launch_agent_failed", fmt.Sprintf("launchctl bootstrap failed (%d diagnostic bytes)", len(output)), err)
		}
		time.Sleep(launchAgentPollInterval)
	}
}

func launchAgentLoaded() bool {
	_, err := launchctl("print", launchServiceTarget())
	return err == nil
}

func RemoveLaunchAgent() error {
	path := LaunchAgentPath()
	_, _ = launchctl("bootout", launchServiceTarget())
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return coded("launch_agent_failed", "could not remove LaunchAgent plist", err)
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, ".launch-agent-*.plist")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}
