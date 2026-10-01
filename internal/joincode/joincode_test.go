package joincode

import (
	"testing"

	"github.com/NTFespolion307/QuorvexFusion/internal/pki"
)

func TestRoundTripAndForgivingInput(t *testing.T) {
	ca, _ := pki.CreateCA("test")
	pin, secret := Pin(ca.Cert), NewSecret()
	code := Format(pin, secret)
	if len(code) != 24 { // 20 chars + 4 dashes
		t.Fatalf("code %q has length %d", code, len(code))
	}
	for _, typed := range []string{code, normalizeForTest(code), " " + code + " "} {
		p, s, err := Parse(typed)
		if err != nil || p != pin || s != secret {
			t.Errorf("Parse(%q) = %q %q %v", typed, p, s, err)
		}
	}
	if !Matches(pin, ca.Cert) {
		t.Error("pin does not match its own CA")
	}
	other, _ := pki.CreateCA("other")
	if Matches(pin, other.Cert) {
		t.Error("pin matches a different CA")
	}
}

// normalizeForTest lower-cases, removes dashes and swaps in look-alikes.
func normalizeForTest(code string) string {
	out := []rune{}
	for _, r := range code {
		switch r {
		case '-':
			continue
		case '0':
			r = 'o'
		case '1':
			r = 'l'
		default:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
		}
		out = append(out, r)
	}
	return string(out)
}

func TestRejects(t *testing.T) {
	for _, bad := range []string{"", "ABC", "cjt_abcd_0123", "7KQ2-MX4P-9TRA-BH3W-C8NU", "7KQ2-MX4P-9TRA-BH3W-C8NE-X"} {
		if Looks(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
