package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/NTFespolion307/QuorvexFusion/internal/secret"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newToken(t *testing.T, s *Store, mod func(*JoinToken)) string {
	t.Helper()
	full, id, hash := secret.New(secret.PrefixJoin)
	tok := &JoinToken{ID: id, SecretHash: hash, CreatedAt: time.Now(), AutoApprove: true}
	if mod != nil {
		mod(tok)
	}
	if err := s.CreateJoinToken(tok); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestJoinTokenUses(t *testing.T) {
	s := openTest(t)
	two := 2
	tok := newToken(t, s, func(j *JoinToken) { j.MaxUses = &two })
	now := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := s.UseJoinToken(tok, now); err != nil {
			t.Fatalf("use %d: %v", i+1, err)
		}
	}
	if _, err := s.UseJoinToken(tok, now); err == nil {
		t.Fatal("third use of a 2-use token succeeded")
	}
}

func TestJoinTokenExpiryRevokeAndBadSecret(t *testing.T) {
	s := openTest(t)
	past := time.Now().Add(-time.Hour)
	expired := newToken(t, s, func(j *JoinToken) { j.ExpiresAt = &past })
	if _, err := s.UseJoinToken(expired, time.Now()); err == nil {
		t.Error("expired token accepted")
	}

	tok := newToken(t, s, nil)
	id, _, _ := secret.Parse(secret.PrefixJoin, tok)
	wrong := tok[:len(tok)-1] + "0"
	if wrong == tok {
		wrong = tok[:len(tok)-1] + "1"
	}
	if _, err := s.UseJoinToken(wrong, time.Now()); !errors.Is(err, ErrBadToken) {
		t.Errorf("wrong secret: err = %v, want ErrBadToken", err)
	}
	if err := s.RevokeJoinToken(id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseJoinToken(tok, time.Now()); err == nil {
		t.Error("revoked token accepted")
	}
	if _, err := s.UseJoinToken("garbage", time.Now()); !errors.Is(err, ErrBadToken) {
		t.Errorf("garbage token: err = %v", err)
	}
}

func TestNodeLifecycle(t *testing.T) {
	s := openTest(t)
	n := &Node{ID: "n1", Name: "box", Status: NodePending, PubKeyFP: "fp1", CSR: []byte{1}, CreatedAt: time.Now(),
		Location: "home", WorkerLabels: map[string]string{"gpu": "4090"}}
	if err := s.CreateNode(n); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveNode("n1", "abc", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.NodeByPubKey("fp1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != NodeApproved || got.CertSerial != "abc" || got.CSR != nil {
		t.Errorf("after approve: %+v", got)
	}
	if l := got.EffectiveLabels(); l["gpu"] != "4090" || l["location"] != "home" {
		t.Errorf("labels = %v", l)
	}

	// Admin labels override worker labels.
	if err := s.SetNodeLabels("n1", map[string]string{"gpu": "3090"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetNode("n1")
	if got.EffectiveLabels()["gpu"] != "3090" {
		t.Error("admin label did not override worker label")
	}

	if err := s.RevokeNode("n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveNode("n1", "def", time.Now()); err == nil {
		t.Error("revoked node was re-approved")
	}
}

func TestAPIToken(t *testing.T) {
	s := openTest(t)
	full, id, hash := secret.New(secret.PrefixAPI)
	if err := s.CreateAPIToken(&APIToken{ID: id, Name: "cli", SecretHash: hash, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckAPIToken(full, time.Now()); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if _, err := s.CheckAPIToken(full+"x", time.Now()); err == nil {
		t.Error("tampered token accepted")
	}
	if err := s.DeleteAPIToken(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckAPIToken(full, time.Now()); err == nil {
		t.Error("deleted token accepted")
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetMeta("k", "v")
	s.Close()
	s, err = Open(path) // migrations must not re-run
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.GetMeta("k"); v != "v" {
		t.Errorf("meta k = %q after reopen", v)
	}
}

func TestLibraryFiles(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	for _, p := range []string{"scene_1/a.blend", "scene_1/tex/b.png", "scene11/c.blend", "solo.txt"} {
		if err := s.PutLibraryFile(&LibraryFile{Path: p, SHA256: "sha-" + p, Size: 10, UploadedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// Re-upload replaces.
	_ = s.PutLibraryFile(&LibraryFile{Path: "solo.txt", SHA256: "new", Size: 20, UploadedAt: now})
	if f, _ := s.LibraryFile("solo.txt"); f.SHA256 != "new" || f.Size != 20 {
		t.Errorf("replace: %+v", f)
	}
	// A folder is a prefix; "_" must not act as a wildcard ("scene11").
	got, _ := s.LibraryFiles("scene_1")
	if len(got) != 2 {
		t.Fatalf("folder scene_1: %d files", len(got))
	}
	if all, _ := s.LibraryFiles(""); len(all) != 4 {
		t.Errorf("all: %d", len(all))
	}
	if n, _ := s.DeleteLibraryFiles("scene_1"); n != 2 {
		t.Errorf("deleted %d", n)
	}
	refs := map[string]bool{}
	_ = s.LibraryBlobs(refs)
	if !refs["sha-scene11/c.blend"] || refs["sha-scene_1/a.blend"] {
		t.Errorf("library blobs %v", refs)
	}
}
