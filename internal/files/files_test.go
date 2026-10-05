package files

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestCleanRel(t *testing.T) {
	ok := map[string]string{"a.txt": "a.txt", "dir/./b": "dir/b", "x/../y": "y", `win\path.txt`: "win/path.txt"}
	for in, want := range ok {
		if got, err := CleanRel(in); err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "..", "../x", "a/../../x", "."} {
		if _, err := CleanRel(bad); err == nil {
			t.Errorf("CleanRel(%q) accepted", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"*.png", "a.png", true},
		{"*.png", "dir/a.png", false},
		{"frames/*", "frames/f_0001.png", true},
		{"frames/*", "frames/sub/x.png", false},
		{"out/**", "out/a/b/c.txt", true},
		{"out/**/*.json", "out/r.json", true},
		{"out/**/*.json", "out/x/y/r.json", true},
		{"**/result.json", "result.json", true},
		{"**/result.json", "a/b/result.json", true},
		{"result.json", "a/result.json", false},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.name); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.pat, c.name, got)
		}
	}
}

func TestCollectOutputs(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(rel), 0o644)
	}
	for _, f := range []string{"frames/1.png", "frames/2.png", "frames/notes.txt", "out/a/b.txt", "log.txt", "keep/x.bin"} {
		write(f)
	}
	if runtime.GOOS != "windows" {
		// A link to a file outside the working directory must not be collected.
		os.Symlink("/etc/hostname", filepath.Join(root, "frames", "evil.png"))
	}
	got, err := CollectOutputs(root, []string{"frames/*.png", "out", "missing/*"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"frames/1.png", "frames/2.png", "out/a/b.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := CollectOutputs(root, []string{"**"}, 3); err != ErrTooMany {
		t.Errorf("limit not enforced: %v", err)
	}
	if _, err := CollectOutputs(root, []string{"../outside/*"}, 10); err == nil {
		t.Error("pattern escaping the working directory accepted")
	}
}
