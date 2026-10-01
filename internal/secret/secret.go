// Package secret creates and checks the bearer secrets used by the cluster:
// join tokens and API tokens.
//
// A token looks like "cjt_<id>_<secret>". The id is stored in plain text so
// a token can be looked up and listed; only a SHA-256 hash of the secret is
// stored. Plain SHA-256 (rather than bcrypt) is fine here because secrets are
// 192 random bits, so they can't be brute-forced from the hash.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	PrefixJoin = "cjt" // join token
	PrefixAPI  = "cat" // API token
)

// New returns a fresh token with the given prefix, its id, and the hash of
// its secret part (what gets stored).
func New(prefix string) (token, id, hash string) {
	id = RandomHex(4)
	sec := RandomHex(24)
	return prefix + "_" + id + "_" + sec, id, Hash(sec)
}

// Parse splits a token into its id and secret, checking the prefix.
func Parse(prefix, token string) (id, sec string, err error) {
	parts := strings.Split(strings.TrimSpace(token), "_")
	if len(parts) != 3 || parts[0] != prefix || parts[1] == "" || parts[2] == "" {
		return "", "", errors.New("malformed token")
	}
	return parts[1], parts[2], nil
}

// Hash returns the stored form of a secret.
func Hash(sec string) string {
	sum := sha256.Sum256([]byte(sec))
	return hex.EncodeToString(sum[:])
}

// Matches compares a secret against a stored hash in constant time.
func Matches(sec, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(Hash(sec)), []byte(hash)) == 1
}

// RandomHex returns n random bytes, hex encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}
