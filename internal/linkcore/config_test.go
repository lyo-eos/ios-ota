package linkcore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) (Config, string) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ios-ota-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	pairRecord := filepath.Join(directory, "remote.plist")
	if err := os.WriteFile(pairRecord, []byte("pair record"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.DeviceLabel = "development iphone"
	config.RemoteIdentifier = "00008140-001A2D3602A0801C"
	config.TargetIP = "100.91.2.3"
	config.PairRecordPath = pairRecord
	return config, filepath.Join(directory, "iphone.json")
}

func TestConfigRoundTripIsOwnerOnly(t *testing.T) {
	config, profilePath := testConfig(t)
	if err := WriteConfig(profilePath, config); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("profile mode = %#o, want 0600", got)
	}
	loaded, err := LoadConfig(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != config {
		t.Fatalf("loaded config = %#v, want %#v", loaded, config)
	}
}

func TestConfigRejectsNonTailnetAddress(t *testing.T) {
	config, _ := testConfig(t)
	config.TargetIP = "203.0.113.10"
	if err := config.Validate(); errorCode(err) != "profile_invalid" {
		t.Fatalf("error = %v, want profile_invalid", err)
	}
}

func TestConfigWireGuardTargetMustBePrivate(t *testing.T) {
	config, _ := testConfig(t)
	config.Overlay = OverlayWireGuard
	for _, address := range []string{"10.100.0.4", "192.168.1.20", "fd00::4"} {
		config.TargetIP = address
		if err := config.Validate(); err != nil {
			t.Fatalf("%s rejected: %v", address, err)
		}
	}
	for _, address := range []string{"203.0.113.10", "100.91.2.3", "127.0.0.1", " 10.100.0.4", ""} {
		config.TargetIP = address
		if err := config.Validate(); errorCode(err) != "profile_invalid" {
			t.Fatalf("%q error = %v, want profile_invalid", address, err)
		}
	}
}

func TestConfigRejectsUnknownOverlay(t *testing.T) {
	config, _ := testConfig(t)
	for _, overlay := range []string{"", "Tailscale", "zerotier"} {
		config.Overlay = overlay
		if err := config.Validate(); errorCode(err) != "profile_invalid" {
			t.Fatalf("overlay %q error = %v, want profile_invalid", overlay, err)
		}
	}
}

func TestLoadConfigUpgradesSchema4AsTailscale(t *testing.T) {
	config, profilePath := testConfig(t)
	legacy := fmt.Sprintf(`{"schema_version":4,"device_label":%q,"remote_identifier":%q,"target_tailnet_ip":"100.64.0.10","remote_pairing_port":49152,"pair_record_path":%q,"local_forward_port":61015,"connect_timeout_seconds":12,"retry_interval_seconds":5,"install_timeout_seconds":1200}`,
		config.DeviceLabel, config.RemoteIdentifier, config.PairRecordPath)
	if err := os.WriteFile(profilePath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != SchemaVersion || loaded.Overlay != OverlayTailscale || loaded.TargetIP != "100.64.0.10" || loaded.InstallTimeoutSeconds != 1200 {
		t.Fatalf("upgraded config = %#v", loaded)
	}
	mixed := strings.TrimSuffix(legacy, "}") + `,"overlay":"wireguard"}`
	if err := os.WriteFile(profilePath, []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(profilePath); errorCode(err) != "profile_invalid" {
		t.Fatalf("schema-4 profile with overlay error = %v, want profile_invalid", err)
	}
}

func TestWireGuardConfigRoundTrip(t *testing.T) {
	config, profilePath := testConfig(t)
	config.Overlay = OverlayWireGuard
	config.TargetIP = "10.100.0.4"
	if err := WriteConfig(profilePath, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "target_tailnet_ip") || !strings.Contains(string(data), `"overlay": "wireguard"`) {
		t.Fatalf("written profile = %s", data)
	}
	loaded, err := LoadConfig(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != config {
		t.Fatalf("loaded config = %#v, want %#v", loaded, config)
	}
}

func TestConfigRejectsGroupReadablePairRecord(t *testing.T) {
	config, _ := testConfig(t)
	if err := os.Chmod(config.PairRecordPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePairRecord(config); errorCode(err) != "pair_record_permissions" {
		t.Fatalf("error = %v, want pair_record_permissions", err)
	}
}

func TestLoadConfigRejectsUnknownAndTrailingJSON(t *testing.T) {
	config, profilePath := testConfig(t)
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.TrimSuffix(string(data), "}") + `,"unexpected":true}`
	if err := os.WriteFile(profilePath, []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(profilePath); errorCode(err) != "profile_invalid" {
		t.Fatalf("unknown field error = %v, want profile_invalid", err)
	}
	if err := os.WriteFile(profilePath, append(data, []byte("\n{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(profilePath); errorCode(err) != "profile_invalid" {
		t.Fatalf("trailing object error = %v, want profile_invalid", err)
	}
}

func TestSocketPathIsProfileScoped(t *testing.T) {
	path, err := SocketPath("/tmp/iphone.json")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/iphone.sock" {
		t.Fatalf("socket path = %q", path)
	}
}

func TestIsOverlayAddress(t *testing.T) {
	for _, address := range []string{"100.64.0.1", "100.127.255.254", "fd7a:115c:a1e0::1234"} {
		if !IsOverlayAddress(OverlayTailscale, address) {
			t.Fatalf("tailscale %s should be accepted", address)
		}
	}
	for _, address := range []string{"100.128.0.1", "192.168.1.1", "invalid"} {
		if IsOverlayAddress(OverlayTailscale, address) {
			t.Fatalf("tailscale %s should be rejected", address)
		}
	}
	for _, address := range []string{"10.100.0.4", "172.16.0.1", "192.168.1.1", "fd12::1"} {
		if !IsOverlayAddress(OverlayWireGuard, address) {
			t.Fatalf("wireguard %s should be accepted", address)
		}
	}
	for _, address := range []string{"100.64.0.1", "8.8.8.8", "127.0.0.1", "0.0.0.0", "::", "fe80::1", "invalid"} {
		if IsOverlayAddress(OverlayWireGuard, address) {
			t.Fatalf("wireguard %s should be rejected", address)
		}
	}
	if IsOverlayAddress("unknown", "10.0.0.1") {
		t.Fatal("unknown overlay accepted an address")
	}
}

func TestWireGuardPingToleratesSleepyPhone(t *testing.T) {
	command, arguments := wireGuardPingCommand("10.100.0.4")
	joined := strings.Join(arguments, " ")
	if command != "/sbin/ping" || !strings.Contains(joined, "-c 3") || !strings.Contains(joined, "-o") {
		t.Fatalf("ping = %s %q", command, joined)
	}
	if command, _ := wireGuardPingCommand("fd00::4"); command != "/sbin/ping6" {
		t.Fatalf("IPv6 ping = %s", command)
	}
}

func TestValidateBuildMetadata(t *testing.T) {
	valid := BuildMetadata{
		Version:        ModuleVersion,
		LinkCoreCommit: PinnedLinkCoreCommit,
		PatchSHA256:    strings.Repeat("a", 64),
	}
	if err := ValidateBuildMetadata(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Version = "dev"
	if err := ValidateBuildMetadata(invalid); errorCode(err) != "build_metadata_invalid" {
		t.Fatalf("error = %v, want build_metadata_invalid", err)
	}
	invalid = valid
	invalid.PatchSHA256 = strings.Repeat("g", 64)
	if err := ValidateBuildMetadata(invalid); errorCode(err) != "build_metadata_invalid" {
		t.Fatalf("error = %v, want build_metadata_invalid", err)
	}
}

func TestTailnetPingAcceptsDERPReachability(t *testing.T) {
	arguments := tailnetPingArguments("100.91.2.3")
	joined := strings.Join(arguments, " ")
	if !strings.Contains(joined, "--until-direct=false") {
		t.Fatalf("ping arguments must treat DERP as reachable: %q", joined)
	}
}
