package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zjz-connect/ios-ota/internal/linkcore"
)

const version = linkcore.ModuleVersion
const linkCoreCommit = linkcore.PinnedLinkCoreCommit

var patchSHA = "unknown"

func emit(value any) {
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func required(value string, name string) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func configure(arguments []string) error {
	flags := flag.NewFlagSet("configure", flag.ContinueOnError)
	profile := flags.String("profile", "", "absolute profile path")
	deviceLabel := flags.String("device-label", "", "display-only device label")
	remoteIdentifier := flags.String("remote-identifier", "", "RemotePairing device identifier")
	target := flags.String("target-tailnet-ip", "", "iPhone Tailnet IP")
	pairRecord := flags.String("pair-record", "", "absolute owner-only RemotePairing plist")
	remotePort := flags.Int("remote-pairing-port", 49152, "RemotePairing TCP port")
	localPort := flags.Int("local-forward-port", 61015, "local userspace forwarder port")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	requiredFlags := []struct {
		value string
		name  string
	}{
		{*profile, "--profile"},
		{*deviceLabel, "--device-label"},
		{*remoteIdentifier, "--remote-identifier"},
		{*target, "--target-tailnet-ip"},
		{*pairRecord, "--pair-record"},
	}
	for _, item := range requiredFlags {
		if err := required(item.value, item.name); err != nil {
			return err
		}
	}
	config := linkcore.DefaultConfig()
	config.DeviceLabel = *deviceLabel
	config.RemoteIdentifier = *remoteIdentifier
	config.TargetTailnetIP = *target
	config.PairRecordPath = *pairRecord
	config.RemotePairingPort = *remotePort
	config.LocalForwardPort = *localPort
	if err := linkcore.ValidatePairRecord(config); err != nil {
		return err
	}
	if err := linkcore.WriteConfig(*profile, config); err != nil {
		return err
	}
	emit(map[string]any{"ok": true, "event": "profile_written", "schema_version": linkcore.SchemaVersion})
	return nil
}

func doctor(arguments []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	profile := flags.String("profile", "", "absolute profile path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	report, err := linkcore.Doctor(ctx, *profile, linkcore.BuildMetadata{
		Version: version, LinkCoreCommit: linkCoreCommit, PatchSHA256: patchSHA,
	})
	if err != nil {
		emit(map[string]any{"ok": false, "event": "doctor", "report": report})
		return err
	}
	emit(map[string]any{"ok": true, "event": "doctor", "report": report})
	return nil
}

func serve(arguments []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	profile := flags.String("profile", "", "absolute profile path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	config, err := linkcore.LoadConfig(*profile)
	if err != nil {
		return err
	}
	if err := linkcore.ValidatePairRecord(config); err != nil {
		return err
	}
	daemon, err := linkcore.NewDaemon(config, *profile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.Serve(ctx)
}

func call(arguments []string, command string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	profile := flags.String("profile", "", "absolute profile path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	config, err := linkcore.LoadConfig(*profile)
	if err != nil {
		return err
	}
	request := linkcore.Request{Command: command}
	if command == "install" {
		if flags.NArg() != 1 {
			return errors.New("install requires exactly one .app path")
		}
		absolute, err := filepath.Abs(flags.Arg(0))
		if err != nil {
			return err
		}
		request.AppPath = absolute
	} else if command == "uninstall" {
		if flags.NArg() != 1 {
			return errors.New("uninstall requires exactly one bundle identifier")
		}
		request.BundleIdentifier = flags.Arg(0)
	}
	timeout := 10 * time.Second
	if command == "install" || command == "uninstall" {
		timeout = config.InstallTimeout() + 30*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return linkcore.Call(ctx, *profile, request, func(response linkcore.Response) { emit(response) })
}

func watch(arguments []string) error {
	flags := flag.NewFlagSet("watch", flag.ContinueOnError)
	profile := flags.String("profile", "", "absolute profile path")
	interval := flags.Duration("interval", time.Second, "status polling interval")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if _, err := linkcore.LoadConfig(*profile); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return linkcore.Watch(ctx, *profile, *interval, func(response linkcore.Response) { emit(response) })
}

func launchAgent(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("launch-agent requires install or remove")
	}
	flags := flag.NewFlagSet("launch-agent "+arguments[0], flag.ContinueOnError)
	profile := flags.String("profile", "", "absolute profile path")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	switch arguments[0] {
	case "install":
		if _, err := linkcore.LoadConfig(*profile); err != nil {
			return err
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		binary, err = filepath.EvalSymlinks(binary)
		if err != nil {
			return err
		}
		if err := linkcore.InstallLaunchAgent(binary, *profile); err != nil {
			return err
		}
		emit(map[string]any{"ok": true, "event": "launch_agent_installed"})
		return nil
	case "remove":
		if err := linkcore.RemoveLaunchAgent(); err != nil {
			return err
		}
		emit(map[string]any{"ok": true, "event": "launch_agent_removed"})
		return nil
	default:
		return errors.New("launch-agent requires install or remove")
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("command required: configure, configure-location, refresh-location, doctor, serve, status, watch, install, uninstall, stop, screenshot, run-tests, launch-agent, version")
	}
	switch arguments[0] {
	case "refresh-location":
		flags := flag.NewFlagSet("refresh-location", flag.ContinueOnError)
		profile := flags.String("profile", "", "absolute profile path")
		output := flags.String("output", "", "new private connection document")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if err := required(*profile, "--profile"); err != nil {
			return err
		}
		if err := required(*output, "--output"); err != nil {
			return err
		}
		if err := linkcore.RefreshLocation(*profile, *output); err != nil {
			return err
		}
		emit(map[string]any{"ok": true, "event": "location_refreshed"})
		return nil
	case "configure-location":
		flags := flag.NewFlagSet("configure-location", flag.ContinueOnError)
		profile := flags.String("profile", "", "absolute profile path")
		listen := flags.String("listen", "", "local Tailnet IP and port")
		output := flags.String("output", "", "new private connection document")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if err := required(*profile, "--profile"); err != nil {
			return err
		}
		if err := required(*listen, "--listen"); err != nil {
			return err
		}
		if err := required(*output, "--output"); err != nil {
			return err
		}
		if err := linkcore.ConfigureLocation(*profile, *listen, *output); err != nil {
			return err
		}
		emit(map[string]any{"ok": true, "event": "location_configured"})
		return nil
	case "run-tests":
		return runTests(arguments[1:])
	case "screenshot":
		return screenshot(arguments[1:])
	case "configure":
		return configure(arguments[1:])
	case "doctor":
		return doctor(arguments[1:])
	case "serve":
		return serve(arguments[1:])
	case "status", "install", "uninstall", "stop":
		return call(arguments[1:], arguments[0])
	case "watch":
		return watch(arguments[1:])
	case "launch-agent":
		return launchAgent(arguments[1:])
	case "version":
		if err := linkcore.ValidateBuildMetadata(linkcore.BuildMetadata{
			Version: version, LinkCoreCommit: linkCoreCommit, PatchSHA256: patchSHA,
		}); err != nil {
			return err
		}
		emit(map[string]any{"version": version, "serviceName": linkcore.ServiceName, "serviceLabel": linkcore.ServiceLabel, "link_core_commit": linkCoreCommit, "patch_sha256": patchSHA})
		return nil
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		code, message := linkcore.ErrorDetails(err)
		emit(map[string]any{"ok": false, "error_code": code, "error": message})
		os.Exit(1)
	}
}
