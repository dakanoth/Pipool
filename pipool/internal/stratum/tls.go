package stratum

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// TLSConfig holds TLS configuration for a Stratum port
type TLSConfig struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`      // separate TLS port, e.g. 3343 alongside 3333
	CertFile string `json:"cert_file"` // path to PEM cert; empty = auto-generate
	KeyFile  string `json:"key_file"`  // path to PEM key;  empty = auto-generate
}

// defaultTLSPort returns the conventional TLS port for a given plain port
func defaultTLSPort(plainPort int) int {
	return plainPort + 10 // e.g. 3333 -> 3343, 3335 -> 3345
}

// buildTLSConfig loads or auto-generates a TLS certificate and returns a *tls.Config
func buildTLSConfig(cfg TLSConfig, symbol string) (*tls.Config, error) {
	var cert tls.Certificate
	var err error

	if cfg.CertFile != "" && cfg.KeyFile != "" {
		// Load from disk
		cert, err = tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("[%s] load TLS keypair: %w", symbol, err)
		}
	} else {
		// Auto-generate self-signed cert (fine for miners on a LAN/VPN)
		cert, err = generateSelfSignedCert(symbol)
		if err != nil {
			return nil, fmt.Errorf("[%s] generate TLS cert: %w", symbol, err)
		}
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// Prefer ECDHE cipher suites for performance on Pi's ARM core
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}, nil
}

// listenTLS creates a TLS-wrapped TCP listener on the given port
func listenTLS(host string, port int, tlsCfg *tls.Config) (net.Listener, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("tls listen on %s: %w", addr, err)
	}
	return ln, nil
}

// generateSelfSignedCert creates an ECDSA P-256 self-signed cert valid for 10 years.
// P-256 is fast on the Pi 5's ARM Cortex-A76 which has hardware AES/SHA acceleration.
func generateSelfSignedCert(symbol string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate ecdsa key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("pipool-%s", symbol),
			Organization: []string{"PiPool Solo Mining"},
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Allow connecting by IP (miners typically use IP, not hostname)
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})

	return tls.X509KeyPair(certPEM, privPEM)
}

// SaveCert writes a TLS cert+key to disk so miners can import the cert for verification
func SaveCert(cert tls.Certificate, certPath, keyPath string) error {
	if len(cert.Certificate) == 0 {
		return fmt.Errorf("empty certificate")
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Certificate[0],
	})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write cert file: %w", err)
	}

	privKey, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("unexpected key type")
	}
	privDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}

	return nil
}
