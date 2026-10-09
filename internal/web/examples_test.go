package web

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The web UI serves copies of the scripts in examples/ (go:embed can't
// reach outside this package). They must not drift apart.
func TestExampleCopiesMatch(t *testing.T) {
	entries, err := os.ReadDir("static/examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		embedded, _ := os.ReadFile(filepath.Join("static/examples", e.Name()))
		original, err := os.ReadFile(filepath.Join("..", "..", "examples", e.Name()))
		if err != nil {
			t.Errorf("%s has no original in examples/: %v", e.Name(), err)
			continue
		}
		if !bytes.Equal(bytes.ReplaceAll(embedded, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(original, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("internal/web/static/examples/%s differs from examples/%s: copy it over", e.Name(), e.Name())
		}
	}
}
