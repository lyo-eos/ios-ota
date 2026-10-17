package linkcore

import "testing"

func TestValidateCommand(t *testing.T) {
	accepted := []Request{
		{Command: "status"},
		{Command: "stop"},
		{Command: "install", AppPath: "/tmp/Test.app"},
		{Command: "uninstall", BundleIdentifier: "lyo.nodus"},
	}
	for _, request := range accepted {
		if err := validateCommand(request, OverlayTailscale); err != nil {
			t.Fatalf("request %#v: %v", request, err)
		}
	}
	rejected := []Request{
		{},
		{Command: "unknown"},
		{Command: "install"},
		{Command: "install", AppPath: "/tmp/Test.app", BundleIdentifier: "lyo.nodus"},
		{Command: "uninstall"},
		{Command: "uninstall", BundleIdentifier: "*"},
		{Command: "uninstall", AppPath: "/tmp/Test.app", BundleIdentifier: "lyo.nodus"},
		{Command: "status", AppPath: "/tmp/Test.app"},
		{Command: "status", BundleIdentifier: "lyo.nodus"},
	}
	for _, request := range rejected {
		if err := validateCommand(request, OverlayTailscale); errorCode(err) != "control_request_invalid" {
			t.Fatalf("request %#v error = %v", request, err)
		}
	}
}
