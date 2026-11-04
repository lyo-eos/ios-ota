package linkcore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidBundleIdentifier(t *testing.T) {
	accepted := []string{"lyo.nodus", "com.example.App-2", "one.zjz.workspace"}
	for _, value := range accepted {
		if !validBundleIdentifier(value) {
			t.Fatalf("valid bundle identifier rejected: %q", value)
		}
	}
	rejected := []string{
		"",
		"single",
		"com..example",
		"com.example.*",
		"com.example app",
		"-com.example",
		"com.example-",
		strings.Repeat("a", 256) + ".example",
	}
	for _, value := range rejected {
		if validBundleIdentifier(value) {
			t.Fatalf("invalid bundle identifier accepted: %q", value)
		}
	}
}

func TestValidateAppAcceptsSignedBundle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign bundle validation is macOS-only")
	}
	directory := t.TempDir()
	appPath := filepath.Join(directory, "NodusRemoteDeployTest.app")
	if err := os.Mkdir(appPath, 0o755); err != nil {
		t.Fatal(err)
	}
	infoPlist := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>NodusRemoteDeployTest</string>
<key>CFBundleIdentifier</key><string>one.zjz.ios-ota-test</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>2.70.0</string>
<key>CFBundleVersion</key><string>1</string>
</dict></plist>`)
	if err := os.WriteFile(filepath.Join(appPath, "Info.plist"), infoPlist, 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(appPath, "NodusRemoteDeployTest")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", appPath).CombinedOutput(); err != nil {
		t.Fatalf("codesign: %v: %s", err, output)
	}
	bundle, err := ValidateApp(context.Background(), appPath)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.BundleIdentifier != "one.zjz.ios-ota-test" || bundle.Path != appPath || bundle.Version != "2.70.0" || bundle.Build != "1" {
		t.Fatalf("bundle = %#v", bundle)
	}
}

func TestValidateAppRejectsUnsignedBundle(t *testing.T) {
	appPath := filepath.Join(t.TempDir(), "Unsigned.app")
	if err := os.Mkdir(appPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appPath, "Info.plist"), []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>one.zjz.unsigned</string><key>CFBundleShortVersionString</key><string>1.0</string><key>CFBundleVersion</key><string>1</string></dict></plist>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateApp(context.Background(), appPath); errorCode(err) != "app_invalid" {
		t.Fatalf("error = %v, want app_invalid", err)
	}
}
