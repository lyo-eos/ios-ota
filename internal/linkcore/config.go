package linkcore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = 5

// Overlays that can carry the RemotePairing tunnel to the iPhone.
const (
	OverlayTailscale = "tailscale"
	OverlayWireGuard = "wireguard"
)

var remoteIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9-]{8,128}$`)
var tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
var tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

type Config struct {
	Location              *LocationConfig `json:"location,omitempty"`
	SchemaVersion         int             `json:"schema_version"`
	DeviceLabel           string          `json:"device_label"`
	RemoteIdentifier      string          `json:"remote_identifier"`
	Overlay               string          `json:"overlay"`
	TargetIP              string          `json:"target_ip"`
	RemotePairingPort     int             `json:"remote_pairing_port"`
	PairRecordPath        string          `json:"pair_record_path"`
	LocalForwardPort      int             `json:"local_forward_port"`
	ConnectTimeoutSeconds int             `json:"connect_timeout_seconds"`
	RetryIntervalSeconds  int             `json:"retry_interval_seconds"`
	InstallTimeoutSeconds int             `json:"install_timeout_seconds"`
}

// profileV4 is the schema before overlays existed; its target was always a
// Tailscale address. LoadConfig upgrades it in memory.
type profileV4 struct {
	Config
	TargetTailnetIP string `json:"target_tailnet_ip"`
}

func DefaultConfig() Config {
	return Config{
		SchemaVersion:         SchemaVersion,
		Overlay:               OverlayTailscale,
		RemotePairingPort:     49152,
		LocalForwardPort:      61015,
		ConnectTimeoutSeconds: 12,
		RetryIntervalSeconds:  5,
		InstallTimeoutSeconds: 300,
	}
}

func (c Config) ConnectTimeout() time.Duration {
	return time.Duration(c.ConnectTimeoutSeconds) * time.Second
}

func (c Config) RetryInterval() time.Duration {
	return time.Duration(c.RetryIntervalSeconds) * time.Second
}

func (c Config) InstallTimeout() time.Duration {
	return time.Duration(c.InstallTimeoutSeconds) * time.Second
}

func validatePort(name string, value int) error {
	if value < 1024 || value > 65535 {
		return coded("profile_invalid", fmt.Sprintf("%s must be in [1024, 65535]", name), nil)
	}
	return nil
}

func validateSeconds(name string, value int, minimum int, maximum int) error {
	if value < minimum || value > maximum {
		return coded("profile_invalid", fmt.Sprintf("%s must be in [%d, %d]", name, minimum, maximum), nil)
	}
	return nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return coded("profile_invalid", fmt.Sprintf("schema_version must be %d", SchemaVersion), nil)
	}
	if strings.TrimSpace(c.DeviceLabel) == "" || len(c.DeviceLabel) > 80 {
		return coded("profile_invalid", "device_label must be 1 to 80 characters", nil)
	}
	if !remoteIdentifierPattern.MatchString(c.RemoteIdentifier) {
		return coded("profile_invalid", "remote_identifier has an invalid format", nil)
	}
	if c.Overlay != OverlayTailscale && c.Overlay != OverlayWireGuard {
		return coded("profile_invalid", `overlay must be "tailscale" or "wireguard"`, nil)
	}
	if !IsOverlayAddress(c.Overlay, c.TargetIP) {
		return coded("profile_invalid", "target_ip must be a Tailscale IP for the tailscale overlay, or a private IP for the wireguard overlay", nil)
	}
	if c.Location != nil {
		if err := c.Location.validate(c.Overlay); err != nil {
			return err
		}
	}
	if err := validatePort("remote_pairing_port", c.RemotePairingPort); err != nil {
		return err
	}
	if err := validatePort("local_forward_port", c.LocalForwardPort); err != nil {
		return err
	}
	if !filepath.IsAbs(c.PairRecordPath) {
		return coded("profile_invalid", "pair_record_path must be absolute", nil)
	}
	if err := validateSeconds("connect_timeout_seconds", c.ConnectTimeoutSeconds, 3, 60); err != nil {
		return err
	}
	if err := validateSeconds("retry_interval_seconds", c.RetryIntervalSeconds, 2, 300); err != nil {
		return err
	}
	if err := validateSeconds("install_timeout_seconds", c.InstallTimeoutSeconds, 30, 1800); err != nil {
		return err
	}
	return nil
}

func requirePrivateRegularFile(path string, missingCode string, permissionsCode string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return coded(missingCode, "required owner-only file is missing", err)
		}
		return coded(missingCode, "could not inspect required file", err)
	}
	if !info.Mode().IsRegular() {
		return coded(permissionsCode, "path must be a regular file", nil)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return coded(permissionsCode, "file must not be accessible by group or other users", nil)
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	if err := requirePrivateRegularFile(path, "profile_missing", "profile_permissions"); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, coded("profile_invalid", "could not read profile", err)
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	_ = json.Unmarshal(data, &header)
	var config Config
	if header.SchemaVersion == 4 {
		var legacy profileV4
		if err := decodeProfile(data, &legacy); err != nil {
			return Config{}, err
		}
		if legacy.Overlay != "" || legacy.TargetIP != "" {
			return Config{}, coded("profile_invalid", "a schema-4 profile cannot set overlay or target_ip", nil)
		}
		config = legacy.Config
		config.SchemaVersion = SchemaVersion
		config.Overlay = OverlayTailscale
		config.TargetIP = legacy.TargetTailnetIP
	} else if err := decodeProfile(data, &config); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func decodeProfile(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return coded("profile_invalid", "could not decode profile", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return coded("profile_invalid", "profile must contain exactly one JSON object", err)
	}
	return nil
}

func ValidatePairRecord(config Config) error {
	return requirePrivateRegularFile(config.PairRecordPath, "pair_record_missing", "pair_record_permissions")
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return coded("profile_write_failed", "could not create profile directory", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return coded("profile_write_failed", "profile parent is not a directory", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return coded("profile_permissions", "profile directory must be owner-only", nil)
	}
	return nil
}

func WriteConfig(path string, config Config) error {
	if !filepath.IsAbs(path) {
		return coded("profile_invalid", "profile path must be absolute", nil)
	}
	if err := config.Validate(); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateDirectory(parent); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".profile-*.json")
	if err != nil {
		return coded("profile_write_failed", "could not create temporary profile", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return coded("profile_write_failed", "could not protect temporary profile", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return coded("profile_write_failed", "could not encode profile", err)
	}
	if err := temporary.Sync(); err != nil {
		return coded("profile_write_failed", "could not sync profile", err)
	}
	if err := temporary.Close(); err != nil {
		return coded("profile_write_failed", "could not close profile", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return coded("profile_write_failed", "could not commit profile", err)
	}
	committed = true
	return nil
}

func SocketPath(profilePath string) (string, error) {
	base := strings.TrimSuffix(filepath.Base(profilePath), filepath.Ext(profilePath))
	path := filepath.Join(filepath.Dir(profilePath), base+".sock")
	if len([]byte(path)) >= 100 {
		return "", coded("control_socket_path", "profile path is too long for a Unix socket", nil)
	}
	return path, nil
}
