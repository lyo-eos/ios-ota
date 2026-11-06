package linkcore

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"howett.net/plist"
)

type AppBundle struct {
	Path string
	AppIdentity
}

type AppIdentity struct {
	BundleIdentifier string `json:"bundle_identifier"`
	Version          string `json:"version"`
	Build            string `json:"build"`
}

type appInfoPlist struct {
	BundleIdentifier string `plist:"CFBundleIdentifier"`
	Version          string `plist:"CFBundleShortVersionString"`
	Build            string `plist:"CFBundleVersion"`
}

func validAppVersion(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

func (identity AppIdentity) valid() bool {
	return validBundleIdentifier(identity.BundleIdentifier) &&
		validAppVersion(identity.Version) && validAppVersion(identity.Build)
}

func validBundleIdentifier(value string) bool {
	if len(value) < 3 || len(value) > 255 {
		return false
	}
	components := strings.Split(value, ".")
	if len(components) < 2 {
		return false
	}
	for _, component := range components {
		if component == "" || component[0] == '-' || component[len(component)-1] == '-' {
			return false
		}
		for _, character := range component {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}

func ValidateApp(ctx context.Context, path string) (AppBundle, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return AppBundle{}, coded("app_invalid", "could not resolve app path", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() || !strings.HasSuffix(strings.ToLower(absolute), ".app") {
		return AppBundle{}, coded("app_invalid", "path must be an existing .app directory", err)
	}
	infoFile, err := os.Open(filepath.Join(absolute, "Info.plist"))
	if err != nil {
		return AppBundle{}, coded("app_invalid", "Info.plist is missing", err)
	}
	defer infoFile.Close()
	const maximumInfoBytes = 1024 * 1024
	infoData, err := io.ReadAll(io.LimitReader(infoFile, maximumInfoBytes+1))
	if err != nil || len(infoData) > maximumInfoBytes {
		return AppBundle{}, coded("app_invalid", "Info.plist could not be read within its size limit", err)
	}
	var appInfo appInfoPlist
	if _, err := plist.Unmarshal(infoData, &appInfo); err != nil {
		return AppBundle{}, coded("app_invalid", "Info.plist is invalid", err)
	}
	identity := AppIdentity{BundleIdentifier: appInfo.BundleIdentifier, Version: appInfo.Version, Build: appInfo.Build}
	if !identity.valid() {
		return AppBundle{}, coded("app_invalid", "Info.plist requires a valid bundle identifier, version and build", nil)
	}
	command := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", absolute)
	if output, err := command.CombinedOutput(); err != nil {
		return AppBundle{}, coded("app_invalid", fmt.Sprintf("code signature verification failed (%d bytes of diagnostic output)", len(output)), err)
	}
	return AppBundle{Path: absolute, AppIdentity: identity}, nil
}
