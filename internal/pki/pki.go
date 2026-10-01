// Package pki implements the cluster's private certificate authority.
//
// The controller owns a self-signed CA. It signs:
//   - its own server certificate (used by both the node port and the web UI),
//   - one client certificate per node, issued when the node joins.
//
// Workers pin the CA: they verify that the controller's certificate chains to
// that exact CA, identified by its SHA-256 fingerprint. Hostname checks are
// deliberately skipped because workers may reach the controller by IP, LAN
// name, public domain, or Tailscale name, and the pinned private CA already
// proves identity.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

const (
	caValidity     = 20 * 365 * 24 * time.Hour
	serverValidity = 2 * 365 * 24 * time.Hour
	nodeValidity   = 10 * 365 * 24 * time.Hour // revocation is enforced by the controller's database
)

// CA is a loaded certificate authority with its signing key.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// NewKey creates a P-256 ECDSA key, used for every key in the cluster.
func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// CreateCA generates a new self-signed CA.
func CreateCA(commonName string) (*CA, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"cluster"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Save writes the CA certificate and key as PEM files.
func (ca *CA) Save(certPath, keyPath string) error {
	if err := WriteCertPEM(certPath, ca.Cert.Raw); err != nil {
		return err
	}
	return WriteKeyPEM(keyPath, ca.Key)
}

// LoadCA reads a CA saved by Save.
func LoadCA(certPath, keyPath string) (*CA, error) {
	cert, err := ReadCertPEM(certPath)
	if err != nil {
		return nil, err
	}
	key, err := ReadKeyPEM(keyPath)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Fingerprint is the CA's identity shown to users, e.g. "sha256:ab12...".
func (ca *CA) Fingerprint() string { return Fingerprint(ca.Cert) }

// Fingerprint returns "sha256:<hex>" of a certificate's DER encoding.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts fingerprints with or without the "sha256:"
// prefix, colons, or uppercase, so users can paste them from anywhere.
func NormalizeFingerprint(fp string) string {
	fp = strings.ToLower(strings.TrimSpace(fp))
	fp = strings.TrimPrefix(fp, "sha256:")
	fp = strings.ReplaceAll(fp, ":", "")
	return "sha256:" + fp
}

// PublicKeyFingerprint identifies a node by its public key.
func PublicKeyFingerprint(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// IssueServerCert creates a TLS server certificate for the controller,
// valid for the given host names and IPs. The returned tls.Certificate
// includes the CA certificate in its chain so clients can see and pin it.
func (ca *CA) IssueServerCert(hosts []string) (tls.Certificate, error) {
	key, err := NewKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "cluster-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der, ca.Cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// SignNodeCSR issues a client certificate for a node. The certificate's
// CommonName is the node ID, which is how the controller identifies the node
// on every connection.
func (ca *CA) SignNodeCSR(csr *x509.CertificateRequest, nodeID string) (*x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"cluster-node"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(nodeValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// CreateCSR builds a certificate signing request for a worker key.
func CreateCSR(key crypto.Signer, hostname string) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: hostname},
	}, key)
}

// ParseCSR parses a CSR and verifies its self-signature, which proves the
// sender holds the private key.
func ParseCSR(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	return csr, nil
}

// SerialString formats a certificate serial number the way it is stored.
func SerialString(cert *x509.Certificate) string { return cert.SerialNumber.Text(16) }

// verifyAgainstCA checks that the peer's leaf certificate chains to ca,
// ignoring the host name (see package comment).
func verifyAgainstCA(peerCerts []*x509.Certificate, ca *x509.Certificate) error {
	if len(peerCerts) == 0 {
		return errors.New("controller presented no certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := peerCerts[0].Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("controller certificate is not signed by the pinned CA: %w", err)
	}
	return nil
}

// PinnedClientTLS returns a TLS client config that only accepts servers whose
// certificate chains to ca. clientCert may be nil (e.g. for the join call).
func PinnedClientTLS(ca *x509.Certificate, clientCert *tls.Certificate) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Standard verification would also check the host name; we replace
		// it with a CA-pinned check in VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyAgainstCA(cs.PeerCertificates, ca)
		},
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return cfg
}

// FetchCA connects to addr and returns the CA certificate the server sends at
// the end of its chain, after checking that the leaf is signed by it. The
// result is NOT trusted yet: the caller must compare its fingerprint with an
// expected value or ask the user to confirm it.
func FetchCA(addr string, timeout time.Duration) (*x509.Certificate, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true, // we verify the chain manually below
		NextProtos:         []string{"h2"},
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) < 2 {
		return nil, errors.New("server did not send a CA certificate; is this a cluster controller?")
	}
	ca := chain[len(chain)-1]
	if !ca.IsCA {
		return nil, errors.New("last certificate in the server chain is not a CA")
	}
	if err := verifyAgainstCA(chain, ca); err != nil {
		return nil, err
	}
	return ca, nil
}

// --- PEM helpers ---

func WriteCertPEM(path string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func ReadCertPEM(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCertPEM(data)
}

func ParseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func CertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// WriteKeyPEM writes a private key readable only by the owner.
func WriteKeyPEM(path string, key crypto.Signer) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
}

func ReadKeyPEM(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM key found", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%s: unsupported key type", path)
	}
	return signer, nil
}
