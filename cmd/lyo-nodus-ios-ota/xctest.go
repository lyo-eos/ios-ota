package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zjz-connect/ios-ota/internal/linkcore"
)

type repeatedStrings []string

func (s *repeatedStrings) String() string         { return strings.Join(*s, ",") }
func (s *repeatedStrings) Set(value string) error { *s = append(*s, value); return nil }

func runTests(arguments []string) error {
	flags := flag.NewFlagSet("run-tests", flag.ContinueOnError)
	profile := flags.String("profile", "", "existing owner-only profile path")
	app := flags.String("app-id", "", "installed target app bundle identifier")
	runner := flags.String("runner-id", "", "installed signed UI test runner bundle identifier")
	bundle := flags.String("test-bundle", "", "embedded test bundle filename ending in .xctest")
	output := flags.String("output", "", "new directory for results and full-frame PNG attachments")
	timeout := flags.Duration("timeout", 5*time.Minute, "test timeout (30s to 30m)")
	captureBind := flags.String("capture-bind-address", "", "local overlay address for screenshot-only requests during native input")
	var tests, attachments repeatedStrings
	flags.Var(&tests, "test", "selected Class/testMethod; repeat for multiple methods")
	flags.Var(&attachments, "require-attachment", "required named PNG; repeat for multiple states")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("run-tests does not accept positional arguments")
	}
	for _, item := range []struct{ value, name string }{{*profile, "--profile"}, {*app, "--app-id"}, {*runner, "--runner-id"}, {*bundle, "--test-bundle"}, {*output, "--output"}} {
		if err := required(item.value, item.name); err != nil {
			return err
		}
	}
	if _, err := linkcore.LoadConfig(*profile); err != nil {
		return err
	}
	absolute, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout+15*time.Second)
	defer cancel()
	return linkcore.Call(ctx, *profile, linkcore.Request{Command: "run-tests", TestRun: &linkcore.TestRunRequest{
		AppID: *app, RunnerID: *runner, TestBundle: *bundle, Tests: tests, RequiredAttachments: attachments,
		OutputDirectory: absolute, TimeoutSeconds: int(*timeout / time.Second), CaptureBindAddress: *captureBind,
	}}, func(response linkcore.Response) { emit(response) })
}
