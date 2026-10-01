package controller

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
	"github.com/NTFespolion307/QuorvexFusion/internal/store"
)

const metaAdminPassword = "admin_password_hash"

// InitResult carries what `cluster controller init` prints for the user.
type InitResult struct {
	Config        *Config
	CAFingerprint string
	JoinToken     string // first join token (auto-approve, 7 days)
	APIToken      string // token for the local CLI
}

// Init creates a new controller data directory: CA, database, admin
// password, config file, a first join token and a CLI API token.
func Init(cfg *Config, adminPassword string) (*InitResult, error) {
	if _, err := os.Stat(cfg.path(configFile)); err == nil {
		return nil, fmt.Errorf("%s is already initialised", cfg.DataDir)
	}
	if len(adminPassword) < 8 {
		return nil, errors.New("admin password must be at least 8 characters")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}

	ca, err := pki.CreateCA("cluster CA")
	if err != nil {
		return nil, err
	}
	if err := ca.Save(cfg.path("ca.crt"), cfg.path("ca.key")); err != nil {
		return nil, err
	}

	st, err := store.Open(cfg.path("cluster.db"))
	if err != nil {
		return nil, err
	}
	defer st.Close()

	if err := setAdminPassword(st, adminPassword); err != nil {
		return nil, err
	}

	res := &InitResult{Config: cfg, CAFingerprint: ca.Fingerprint()}
	expires := time.Now().Add(7 * 24 * time.Hour)
	res.JoinToken, _, err = createJoinToken(st, JoinTokenOptions{
		Description: "created by init", AutoApprove: true, ExpiresAt: &expires,
	})
	if err != nil {
		return nil, err
	}
	res.APIToken, _, err = createAPIToken(st, "local-cli")
	if err != nil {
		return nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	return res, nil
}

// SetAdminPassword changes the admin password in an existing data dir.
func SetAdminPassword(dataDir, password string) error {
	if len(password) < 8 {
		return errors.New("admin password must be at least 8 characters")
	}
	cfg, err := LoadConfig(dataDir)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.path("cluster.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	return setAdminPassword(st, password)
}

func setAdminPassword(st *store.Store, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return st.SetMeta(metaAdminPassword, string(hash))
}
