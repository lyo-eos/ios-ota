package linkcore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type LocationConfig struct {
	Listen      string `json:"listen"`
	Credentials string `json:"credentials"`
}
type locationCredentials struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
	Token       string `json:"token"`
}
type LocationConnection struct {
	URL               string `json:"url"`
	CertificateSHA256 string `json:"certificateSHA256"`
	Token             string `json:"token"`
}

func (c LocationConfig) validate(overlay string) error {
	host, port, err := net.SplitHostPort(c.Listen)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1024 || portNumber > 65535 || net.ParseIP(host).To4() == nil || !validCaptureBindAddress(overlay, host) || !filepath.IsAbs(c.Credentials) {
		return errors.New("location requires a local " + overlay + " IPv4 address, explicit port and absolute credential path")
	}
	return nil
}
func writeLocationFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}
func ConfigureLocation(profile, listen, output string) error {
	config, err := LoadConfig(profile)
	if err != nil {
		return err
	}
	if config.Location != nil {
		return errors.New("location is already configured")
	}
	credentialsPath := filepath.Join(filepath.Dir(profile), "ios-ota-location.json")
	location := LocationConfig{Listen: listen, Credentials: credentialsPath}
	if err := location.validate(config.Overlay); err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(listen)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "iOS OTA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(host)}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	pkcs, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	credentials := locationCredentials{Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs})), Token: token}
	digest := sha256.Sum256(der)
	connection := LocationConnection{URL: "https://" + listen, CertificateSHA256: hex.EncodeToString(digest[:]), Token: token}
	if err := writeLocationFile(credentialsPath, credentials); err != nil {
		return err
	}
	if err := writeLocationFile(output, connection); err != nil {
		os.Remove(credentialsPath)
		return err
	}
	config.Location = &location
	if err := WriteConfig(profile, config); err != nil {
		os.Remove(credentialsPath)
		os.Remove(output)
		return err
	}
	return nil
}
func startLocationHTTPS(ctx context.Context, overlay string, config LocationConfig, session *locationSession, active func() bool) (*http.Server, error) {
	if err := config.validate(overlay); err != nil {
		return nil, err
	}
	if err := requirePrivateRegularFile(config.Credentials, "location_credentials_missing", "location_credentials_permissions"); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(config.Credentials)
	if err != nil {
		return nil, err
	}
	var credentials locationCredentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, err
	}
	if len(credentials.Token) != 43 {
		return nil, errors.New("invalid location credential")
	}
	cert, err := tls.X509KeyPair([]byte(credentials.Certificate), []byte(credentials.PrivateKey))
	if err != nil {
		return nil, err
	}
	listener, err := tls.Listen("tcp", config.Listen, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: locationHandler(ctx, credentials.Token, session, active), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 4096}
	context.AfterFunc(ctx, func() { server.Close() })
	go func() { _ = server.Serve(listener) }()
	return server, nil
}
func locationHandler(owner context.Context, token string, session *locationSession, active func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		expected := "Bearer " + token
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path != "/v1/location" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodDelete {
			w.WriteHeader(405)
			return
		}
		if owner.Err() != nil {
			w.WriteHeader(503)
			return
		}
		var err error
		code := http.StatusOK
		if r.Method == http.MethodPut {
			var coordinate struct {
				Latitude  *float64 `json:"latitude"`
				Longitude *float64 `json:"longitude"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&coordinate)
			if err == nil && decoder.Decode(&struct{}{}) != io.EOF {
				err = errors.New("invalid trailing data")
			}
			if err != nil || coordinate.Latitude == nil || coordinate.Longitude == nil || !validLocation(*coordinate.Latitude, *coordinate.Longitude) {
				http.Error(w, "invalid coordinate", 400)
				return
			}
			// Once accepted, a mutation belongs to the host, not the phone HTTP request.
			ctx, cancel := context.WithTimeout(owner, 10*time.Second)
			err = session.set(ctx, *coordinate.Latitude, *coordinate.Longitude)
			cancel()
		} else if r.Method == http.MethodDelete {
			if r.ContentLength != 0 {
				http.Error(w, "unexpected body", 400)
				return
			}
			ctx, cancel := context.WithTimeout(owner, 10*time.Second)
			err = session.clear(ctx)
			cancel()
		}
		state := session.snapshot(active())
		if err != nil {
			code = http.StatusBadGateway
			state.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(state)
	})
}
