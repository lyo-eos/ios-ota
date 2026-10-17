package linkcore

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

type bridgeCloseListener struct {
	net.Listener
	closes atomic.Int32
}

func (l *bridgeCloseListener) Close() error { l.closes.Add(1); return nil }

func TestCaptureBridgeCloseAndCancellationAreIdempotent(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		listener := &bridgeCloseListener{}
		bridge := &captureBridge{server: &http.Server{}, listener: listener}
		done := make(chan struct{})
		if attempt%2 == 0 {
			cancel()
		}
		context.AfterFunc(ctx, func() { bridge.Close(); close(done) })
		var workers sync.WaitGroup
		for i := 0; i < 8; i++ {
			workers.Add(1)
			go func() { defer workers.Done(); bridge.Close(); cancel() }()
		}
		workers.Wait()
		cancel()
		<-done
		if listener.closes.Load() != 1 {
			t.Fatal("listener must close exactly once across cancellation and explicit shutdown")
		}
	}
}

func bridgeRequest(handler http.Handler, token, method, path string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
func TestCaptureBridgeRetainsOriginalAndLimitsAuthority(t *testing.T) {
	var original bytes.Buffer
	_ = png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 3, 5)))
	var calls atomic.Int32
	handler := captureBridgeHandler(context.Background(), "run-secret", func(context.Context) ([]byte, error) { calls.Add(1); return original.Bytes(), nil })
	invalid := []struct {
		token, method, path string
		body                []byte
	}{{"wrong", "POST", "/screenshot", nil}, {"run-secret", "GET", "/screenshot", nil}, {"run-secret", "POST", "/launch", nil}, {"run-secret", "POST", "/screenshot?x=1", nil}, {"run-secret", "POST", "/screenshot", []byte("body")}}
	for _, request := range invalid {
		if bridgeRequest(handler, request.token, request.method, request.path, request.body).Code == http.StatusOK {
			t.Fatal("invalid authority accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached native session")
	}
	response := bridgeRequest(handler, "run-secret", "POST", "/screenshot", nil)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), original.Bytes()) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("original byte preservation failed")
	}
}
func TestCaptureBridgeRejectsConcurrentAndClosedRuns(t *testing.T) {
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := captureBridgeHandler(owner, "secret", func(context.Context) ([]byte, error) { close(started); <-release; return nil, nil })
	go func() { defer close(done); bridgeRequest(handler, "secret", "POST", "/screenshot", nil) }()
	<-started
	if bridgeRequest(handler, "secret", "POST", "/screenshot", nil).Code != http.StatusConflict {
		t.Fatal("concurrent request accepted")
	}
	close(release)
	<-done
	cancel()
	if bridgeRequest(handler, "secret", "POST", "/screenshot", nil).Code != http.StatusGone {
		t.Fatal("closed run accepted")
	}
}
func TestCaptureBridgeRejectsInvalidImageAndBind(t *testing.T) {
	handler := captureBridgeHandler(context.Background(), "secret", func(context.Context) ([]byte, error) { return []byte("not an image"), nil })
	if bridgeRequest(handler, "secret", "POST", "/screenshot", nil).Code != http.StatusBadGateway {
		t.Fatal("invalid image accepted")
	}
	for _, address := range []string{"", "0.0.0.0", "127.0.0.1", "192.168.1.1", "::", "100.128.0.1", "example.com"} {
		if validCaptureBindAddress(OverlayTailscale, address) {
			t.Fatalf("unsafe bind %s", address)
		}
	}
	for _, address := range []string{"100.64.0.1", "100.127.255.254", "fd7a:115c:a1e0::1"} {
		if !validCaptureBindAddress(OverlayTailscale, address) {
			t.Fatalf("valid tailnet bind rejected %s", address)
		}
	}
	for _, address := range []string{"", "0.0.0.0", "127.0.0.1", "::", "203.0.113.1", "example.com"} {
		if validCaptureBindAddress(OverlayWireGuard, address) {
			t.Fatalf("unsafe wireguard bind %s", address)
		}
	}
	if !validCaptureBindAddress(OverlayWireGuard, "192.168.50.10") {
		t.Fatal("private LAN bind rejected for wireguard")
	}
	if _, err := startCaptureBridge(context.Background(), OverlayWireGuard, "0.0.0.0", nil); errorCode(err) != "capture_bind_invalid" {
		t.Fatalf("wildcard bind error = %v, want capture_bind_invalid", err)
	}
}
