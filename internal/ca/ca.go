package ca

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	caValidityDays  = 5 * 365
	hostValidityDays = 365
)

// EnsureCA ensures a CA certificate and key exist on disk.
// Returns (caCertPath, caKeyPath). Creates them if they don't exist.
func EnsureCA(caDir string) (string, string, error) {
	caCertPath := filepath.Join(caDir, "ca.pem")
	caKeyPath := filepath.Join(caDir, "ca-key.pem")

	if _, err := os.Stat(caCertPath); err == nil {
		if _, err := os.Stat(caKeyPath); err == nil {
			// Validate existing files are loadable
			if _, _, err := loadCA(caCertPath, caKeyPath); err == nil {
				return caCertPath, caKeyPath, nil
			}
		}
	}

	if err := os.MkdirAll(caDir, 0700); err != nil {
		return "", "", err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate CA key: %w", err)
	}

	now := time.Now().UTC()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "claude-tap CA",
			Organization: []string{"claude-tap"},
		},
		NotBefore:             now,
		NotAfter:              now.Add(caValidityDays * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("create CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	if err := os.WriteFile(caKeyPath, keyPEM, 0600); err != nil {
		return "", "", fmt.Errorf("write CA key: %w", err)
	}
	if err := os.WriteFile(caCertPath, certPEM, 0644); err != nil {
		return "", "", fmt.Errorf("write CA cert: %w", err)
	}

	return caCertPath, caKeyPath, nil
}

func loadCA(caCertPath, caKeyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}

	keyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("invalid CA key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}

// CA generates per-host TLS certificates signed by the CA.
type CA struct {
	cert      *x509.Certificate
	key       *rsa.PrivateKey
	hostCache map[string]*tlsCertPair
	mu        sync.Mutex
}

type tlsCertPair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA loads an existing CA from disk.
func NewCA(caCertPath, caKeyPath string) (*CA, error) {
	cert, key, err := loadCA(caCertPath, caKeyPath)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert:      cert,
		key:       key,
		hostCache: make(map[string]*tlsCertPair),
	}, nil
}

// GetHostCertPEM returns (certPEM, keyPEM) for the given hostname.
// Generates and caches a new certificate signed by the CA if needed.
func (c *CA) GetHostCertPEM(hostname string) ([]byte, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if pair, ok := c.hostCache[hostname]; ok {
		return pair.CertPEM, pair.KeyPEM, nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate host key: %w", err)
	}

	now := time.Now().UTC()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		NotBefore: now,
		NotAfter:  now.Add(hostValidityDays * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Add SAN: use IPAddress for IP addresses, DNSName for hostnames
	if ip := net.ParseIP(hostname); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{hostname}
	}

	// Authority Key Identifier from CA's public key
	pubKeyHash := sha256.Sum256(x509.MarshalPKCS1PublicKey(&c.key.PublicKey))
	template.AuthorityKeyId = pubKeyHash[:]
	// Subject Key Identifier from host key
	hostPubHash := sha256.Sum256(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	template.SubjectKeyId = hostPubHash[:]

	certDER, err := x509.CreateCertificate(rand.Reader, template, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create host cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	pair := &tlsCertPair{CertPEM: certPEM, KeyPEM: keyPEM}
	c.hostCache[hostname] = pair

	return certPEM, keyPEM, nil
}
