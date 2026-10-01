// Package joincode implements short, typeable join codes such as
//
//	7KQ2-MX4P-9TRA-BH3W-C8NE
//
// A code is 20 characters of Crockford base32 (no I, L, O or U, so it is
// hard to misread), 100 bits in two halves:
//
//   - pin (first 10 chars, 50 bits): the start of the controller's CA
//     fingerprint. The worker checks the controller's certificate against
//     it BEFORE sending anything, so nobody has to compare long
//     fingerprints by hand. Impersonating the controller would need a CA
//     certificate whose SHA-256 matches those 50 bits: years of computing.
//   - secret (last 10 chars, 50 bits): the join token's secret. It is only
//     sent to a verified controller, and guesses are rate-limited there.
package joincode

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"math/big"
	"strings"
)

// alphabet is Crockford's base32.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	PinLen    = 10
	SecretLen = 10
	codeLen   = PinLen + SecretLen
)

// Pin is the code prefix that identifies a CA certificate.
func Pin(ca *x509.Certificate) string {
	sum := sha256.Sum256(ca.Raw)
	// The first 50 bits of the fingerprint, 5 bits per character.
	n := new(big.Int).SetBytes(sum[:7]) // 56 bits
	n.Rsh(n, 56-5*PinLen)
	return encode(n, PinLen)
}

// NewSecret returns a random secret part.
func NewSecret() string {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 5*SecretLen))
	if err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return encode(n, SecretLen)
}

func encode(n *big.Int, length int) string {
	b := make([]byte, length)
	v := new(big.Int).Set(n)
	mask := big.NewInt(31)
	for i := length - 1; i >= 0; i-- {
		b[i] = alphabet[new(big.Int).And(v, mask).Int64()]
		v.Rsh(v, 5)
	}
	return string(b)
}

// Format joins pin and secret into the displayed form with dashes.
func Format(pin, secret string) string {
	s := pin + secret
	var parts []string
	for i := 0; i < len(s); i += 4 {
		parts = append(parts, s[i:min(i+4, len(s))])
	}
	return strings.Join(parts, "-")
}

// normalize upper-cases, drops separators, and maps look-alike characters
// the way Crockford base32 specifies (O->0, I/L->1).
func normalize(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch {
		case r == '-' || r == ' ' || r == '_':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		b.WriteRune(r)
	}
	return b.String()
}

var ErrInvalid = errors.New("not a valid join code (expected 20 characters like 7KQ2-MX4P-9TRA-BH3W-C8NE)")

// Parse splits a user-typed code into its pin and secret.
func Parse(code string) (pin, secret string, err error) {
	s := normalize(code)
	if len(s) != codeLen {
		return "", "", ErrInvalid
	}
	for _, r := range s {
		if !strings.ContainsRune(alphabet, r) {
			return "", "", ErrInvalid
		}
	}
	return s[:PinLen], s[PinLen:], nil
}

// Looks reports whether s looks like a join code (rather than a long
// cjt_ token), so one flag can accept both.
func Looks(s string) bool {
	_, _, err := Parse(s)
	return err == nil
}

// Matches reports whether ca is the certificate a code's pin refers to.
func Matches(pin string, ca *x509.Certificate) bool {
	return Pin(ca) == pin
}
