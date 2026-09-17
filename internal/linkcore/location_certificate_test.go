package linkcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshLocationAfterAddressChangePreservesAuthority(t *testing.T) {
	config, profile := testConfig(t)
	if err := WriteConfig(profile, config); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(filepath.Dir(profile), "connection-first.json")
	if err := ConfigureLocation(profile, "100.87.87.49:61443", first); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(profile)
	if err != nil {
		t.Fatal(err)
	}
	readCredentials := func() locationCredentials {
		t.Helper()
		data, err := os.ReadFile(config.Location.Credentials)
		if err != nil {
			t.Fatal(err)
		}
		var credentials locationCredentials
		if err := json.Unmarshal(data, &credentials); err != nil {
			t.Fatal(err)
		}
		return credentials
	}
	before := readCredentials()
	config.Location.Listen = "100.87.87.50:61443"
	if err := WriteConfig(profile, config); err != nil {
		t.Fatal(err)
	}
	profileBefore, _ := os.ReadFile(profile)
	if _, err := startLocationHTTPS(context.Background(), *config.Location, nil, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("address mismatch not rejected: %v", err)
	}
	next := filepath.Join(filepath.Dir(profile), "connection-current.json")
	if err := RefreshLocation(profile, next); err != nil {
		t.Fatal(err)
	}
	after := readCredentials()
	if before.PrivateKey != after.PrivateKey || before.Token != after.Token {
		t.Fatal("refresh replaced existing authority")
	}
	profileAfter, _ := os.ReadFile(profile)
	if !bytes.Equal(profileBefore, profileAfter) {
		t.Fatal("refresh changed device profile")
	}
	pair, err := tls.X509KeyPair([]byte(after.Certificate), []byte(after.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := pair.Leaf.VerifyHostname("100.87.87.50"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(next)
	if err != nil {
		t.Fatal(err)
	}
	var connection LocationConnection
	if err := json.Unmarshal(data, &connection); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(pair.Certificate[0])
	if connection.CertificateSHA256 != hex.EncodeToString(digest[:]) || connection.Token != before.Token || connection.URL != "https://100.87.87.50:61443" {
		t.Fatal("connection does not describe refreshed certificate")
	}
	for _, path := range []string{next, config.Location.Credentials} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credential permissions changed")
		}
	}
	if err := RefreshLocation(profile, next); err == nil {
		t.Fatal("existing export overwritten")
	}
	if final := readCredentials(); final.Certificate != after.Certificate {
		t.Fatal("failed export modified active certificate")
	}
}
