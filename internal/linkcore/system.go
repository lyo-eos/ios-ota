package linkcore

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const ModuleVersion = "1.4.0"
const PinnedLinkCoreCommit = "3ebc297691a9e364772aef027744ebc0c49421a5"

type DoctorReport struct {
	MacOS                bool   `json:"macos"`
	Arm64                bool   `json:"arm64"`
	BuildMetadataValid   bool   `json:"build_metadata_valid"`
	BuildVersion         string `json:"build_version"`
	LinkCoreCommit       string `json:"link_core_commit"`
	PatchSHA256          string `json:"patch_sha256"`
	ProfileValid         bool   `json:"profile_valid"`
	PairRecordValid      bool   `json:"pair_record_valid"`
	TailnetPeerPresent   bool   `json:"tailnet_peer_present"`
	TailnetReachable     bool   `json:"tailnet_reachable"`
	RemoteListenerOpen   bool   `json:"remote_listener_open"`
	RemoteListenerStatus string `json:"remote_listener_status"`
}

type BuildMetadata struct {
	Version        string
	LinkCoreCommit string
	PatchSHA256    string
}

type tailscaleStatus struct {
	Peer map[string]struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Peer"`
}

func findTailscale() (string, error) {
	path, err := exec.LookPath("tailscale")
	if err == nil {
		return path, nil
	}
	for _, candidate := range []string{
		"/Applications/Tailscale.app/Contents/MacOS/tailscale",
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	} {
		if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", coded("tailnet_unreachable", "tailscale CLI is missing", err)
}

func validateTailnet(ctx context.Context, target string) (bool, bool, error) {
	tailscale, err := findTailscale()
	if err != nil {
		return false, false, err
	}
	statusCommand := exec.CommandContext(ctx, tailscale, "status", "--json")
	statusBytes, err := statusCommand.Output()
	if err != nil {
		return false, false, coded("tailnet_unreachable", "tailscale status failed", err)
	}
	var status tailscaleStatus
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return false, false, coded("tailnet_unreachable", "tailscale status was invalid", err)
	}
	present := false
	online := false
	for _, peer := range status.Peer {
		for _, address := range peer.TailscaleIPs {
			if address == target {
				present = true
				online = peer.Online
			}
		}
	}
	if !present || !online {
		return present, false, coded("tailnet_unreachable", "configured iPhone peer is absent or offline", nil)
	}
	pingCommand := exec.CommandContext(ctx, tailscale, tailnetPingArguments(target)...)
	if err := pingCommand.Run(); err != nil {
		return true, false, coded("tailnet_unreachable", "tailscale ping failed", err)
	}
	return true, true, nil
}

func tailnetPingArguments(target string) []string {
	return []string{"ping", "--c", "1", "--timeout", "5s", "--until-direct=false", target}
}

func listenerOpen(ctx context.Context, config Config) bool {
	dialer := net.Dialer{Timeout: config.ConnectTimeout()}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(config.TargetTailnetIP, itoa(config.RemotePairingPort)))
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

// ValidateBuildMetadata checks the actual executable before version/doctor success.
func ValidateBuildMetadata(metadata BuildMetadata) error {
	if metadata.Version != ModuleVersion || metadata.LinkCoreCommit != PinnedLinkCoreCommit {
		return coded("build_metadata_invalid", "binary version or Link Core commit does not match the pinned module", nil)
	}
	if len(metadata.PatchSHA256) != 64 {
		return coded("build_metadata_invalid", "binary has no valid downstream patch hash", nil)
	}
	for _, character := range metadata.PatchSHA256 {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return coded("build_metadata_invalid", "binary downstream patch hash is malformed", nil)
		}
	}
	return nil
}

func Doctor(ctx context.Context, profilePath string, metadata BuildMetadata) (DoctorReport, error) {
	report := DoctorReport{
		MacOS:          runtime.GOOS == "darwin",
		Arm64:          runtime.GOARCH == "arm64",
		BuildVersion:   metadata.Version,
		LinkCoreCommit: metadata.LinkCoreCommit,
		PatchSHA256:    metadata.PatchSHA256,
	}
	if !report.MacOS || !report.Arm64 {
		return report, coded("platform_unsupported", ServiceName+" requires macOS arm64", nil)
	}
	if err := ValidateBuildMetadata(metadata); err != nil {
		return report, err
	}
	report.BuildMetadataValid = true
	config, err := LoadConfig(profilePath)
	if err != nil {
		return report, err
	}
	report.ProfileValid = true
	if err := ValidatePairRecord(config); err != nil {
		return report, err
	}
	report.PairRecordValid = true
	present, reachable, err := validateTailnet(ctx, config.TargetTailnetIP)
	report.TailnetPeerPresent = present
	report.TailnetReachable = reachable
	if err != nil {
		return report, err
	}
	report.RemoteListenerOpen = listenerOpen(ctx, config)
	if report.RemoteListenerOpen {
		report.RemoteListenerStatus = "open"
	} else {
		report.RemoteListenerStatus = "cold"
	}
	return report, nil
}

func IsTailnetAddress(value string) bool {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	return err == nil && (tailscaleIPv4.Contains(address) || tailscaleIPv6.Contains(address))
}
