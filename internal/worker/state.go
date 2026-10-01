package worker

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
)

// The worker's identity lives in its data directory:
//
//	node.key     private key, generated on first join, never leaves the machine
//	node.crt     certificate issued by the controller (absent while pending)
//	ca.crt       the controller's CA, pinned at join time
//	worker.json  controller address and node ID

type state struct {
	Controller string `json:"controller"`
	NodeID     string `json:"node_id"`
}

type identity struct {
	dir string
}

func (id identity) path(name string) string { return filepath.Join(id.dir, name) }

func (id identity) loadState() (*state, error) {
	data, err := os.ReadFile(id.path("worker.json"))
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (id identity) saveState(st *state) error {
	data, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(id.path("worker.json"), append(data, '\n'), 0o600)
}

// key loads the node key, creating it on first use.
func (id identity) key() (crypto.Signer, error) {
	k, err := pki.ReadKeyPEM(id.path("node.key"))
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(id.dir, 0o700); err != nil {
		return nil, err
	}
	nk, err := pki.NewKey()
	if err != nil {
		return nil, err
	}
	if err := pki.WriteKeyPEM(id.path("node.key"), nk); err != nil {
		return nil, err
	}
	return nk, nil
}

func (id identity) ca() (*x509.Certificate, error) { return pki.ReadCertPEM(id.path("ca.crt")) }

func (id identity) hasCert() bool {
	_, err := os.Stat(id.path("node.crt"))
	return err == nil
}

// tlsCert returns the node key and certificate as a TLS client certificate.
func (id identity) tlsCert() (*tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(id.path("node.crt"), id.path("node.key"))
	if err != nil {
		return nil, err
	}
	return &c, nil
}
