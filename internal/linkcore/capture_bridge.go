package linkcore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"image/png"
	"net"
	"net/http"
	"sync"
	"time"
)

type captureBridge struct {
	URL       string
	Token     string
	server    *http.Server
	listener  net.Listener
	closeOnce sync.Once
}

// validCaptureBindAddress keeps phone-facing listeners off wildcard and
// loopback binds: a Tailscale IP, or with WireGuard the Mac's private LAN IP
// that the tunnel routes to.
func validCaptureBindAddress(overlay string, value string) bool {
	return IsOverlayAddress(overlay, value)
}

func startCaptureBridge(ctx context.Context, overlay string, address string, take func(context.Context) ([]byte, error)) (*captureBridge, error) {
	if !validCaptureBindAddress(overlay, address) {
		return nil, coded("capture_bind_invalid", "a local "+overlay+" address is required", nil)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		return nil, coded("capture_bind_failed", "could not bind the selected local "+overlay+" address", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		listener.Close()
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	bridge := &captureBridge{URL: "http://" + listener.Addr().String() + "/screenshot", Token: token, listener: listener}
	bridge.server = &http.Server{Handler: captureBridgeHandler(ctx, token, take), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 3 * time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
	context.AfterFunc(ctx, bridge.Close)
	go func() { _ = bridge.server.Serve(listener) }()
	return bridge, nil
}

func (b *captureBridge) Close() {
	b.closeOnce.Do(func() {
		_ = b.server.Close()
		_ = b.listener.Close()
	})
}

func captureBridgeHandler(owner context.Context, token string, take func(context.Context) ([]byte, error)) http.Handler {
	var captureMu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		expected := "Bearer " + token
		if len(r.Header.Get("Authorization")) != len(expected) || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/screenshot" || r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "invalid screenshot request", http.StatusBadRequest)
			return
		}
		if owner.Err() != nil {
			http.Error(w, "capture run closed", http.StatusGone)
			return
		}
		if !captureMu.TryLock() {
			http.Error(w, "screenshot in progress", http.StatusConflict)
			return
		}
		defer captureMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		stop := context.AfterFunc(owner, cancel)
		defer stop()
		data, err := take(ctx)
		if err == nil {
			err = validateCapturePNG(data)
		}
		if err != nil {
			http.Error(w, "native capture failed", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
}

func validateCapturePNG(data []byte) error {
	if len(data) == 0 || len(data) > maxScreenshotBytes {
		return errors.New("invalid image size")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 16384 || config.Height > 16384 || int64(config.Width)*int64(config.Height) > maxScreenshotPixels {
		return errors.New("invalid native PNG dimensions")
	}
	_, err = png.Decode(bytes.NewReader(data))
	return err
}
