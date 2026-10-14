package catalog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixture() Candidate {
	return Candidate{Files: []File{
		{Path: "context/routing.md", Content: "---\ntitle: Customer request path\naliases: [CP]\n---\n# Routing\nRequests pass through a gateway to Kubernetes.\n"},
		{Path: "context/other.md", Content: "# Elsewhere\nAn unrelated platform.\n"},
		{Path: "skills/triage/SKILL.md", Content: "---\nname: triage\ndescription: Investigate an alert.\n---\n# Triage\nRead [queries](references/queries.md).\n"},
		{Path: "skills/triage/references/queries.md", Content: "# Queries\nLook up service.name.\n"},
	}}
}

func TestSearchAndRead(t *testing.T) {
	s, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.List(Query{Query: "cp", Kind: "context"})
	if err != nil || r.Total != 1 || r.Items[0].Path != "context/routing.md" {
		t.Fatalf("alias lookup: %+v, %v", r, err)
	}
	r, err = s.List(Query{Kind: "skill"})
	if err != nil || r.Total != 1 || r.Items[0].Path != "skills/triage/SKILL.md" {
		t.Fatalf("skill lookup: %+v, %v", r, err)
	}
	r, err = s.List(Query{Limit: 2})
	if err != nil || r.NextOffset == nil || *r.NextOffset != 2 || r.Total != 4 {
		t.Fatalf("pagination: %+v, %v", r, err)
	}
	next, err := s.List(Query{Limit: 2, Offset: *r.NextOffset})
	if err != nil || next.NextOffset != nil || len(next.Items) != 2 || next.Items[0].Path == r.Items[0].Path {
		t.Fatalf("next page: %+v, %v", next, err)
	}
	for _, q := range []Query{{Kind: "pod"}, {Limit: -1}, {Limit: 101}, {Offset: -1}, {Query: strings.Repeat("x", 1001)}} {
		if _, err := s.List(q); err == nil {
			t.Fatalf("accepted invalid query: %+v", q)
		}
	}
	d, err := s.Read("skills/triage/references/queries.md")
	if err != nil || !strings.Contains(d.Content, "service.name") {
		t.Fatalf("reference read: %+v, %v", d, err)
	}
	for _, p := range []string{"../secret.md", "context/../../etc/passwd", "/context/routing.md", "go.mod"} {
		if _, err := s.Read(p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("read unexpected path: %s: %v", p, err)
		}
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name string
		c    Candidate
	}{
		{"empty", Candidate{}},
		{"invalid YAML", Candidate{Files: []File{{Path: "context/a.md", Content: "---\naliases: [broken\n---\ntext"}}}},
		{"unterminated", Candidate{Files: []File{{Path: "context/a.md", Content: "---\ntitle: Hello"}}}},
		{"duplicate", Candidate{Files: []File{{Path: "context/a.md", Content: "a"}, {Path: "context/a.md", Content: "b"}}}},
		{"orphan reference", Candidate{Files: []File{{Path: "skills/missing/references/a.md", Content: "a"}}}},
		{"skill mismatch", Candidate{Files: []File{{Path: "skills/triage/SKILL.md", Content: "---\nname: other\ndescription: test\n---\ntext"}}}},
		{"binary", Candidate{Files: []File{{Path: "context/a.md", Content: "a\x00b"}}}},
		{"oversize", Candidate{Files: []File{{Path: "context/a.md", Content: strings.Repeat("x", MaxFileBytes+1)}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build(tt.c); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestLocalSourceFiltersAndRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "context/a.md", "# Hello")
	writeFile(t, dir, "private.md", "secret")
	writeFile(t, dir, "context/.secret.md", "secret")
	writeFile(t, dir, "context/password.txt", "secret")
	src := LocalSource{Dir: dir}
	c, err := src.Load(context.Background())
	if err != nil || len(c.Files) != 1 {
		t.Fatalf("unexpected published files: %+v, %v", c, err)
	}
	external := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(external, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "context/link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Load(context.Background()); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestRefreshFailureAndRestartCache(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "context/a.md", "# First\nOriginal.")
	cache := filepath.Join(t.TempDir(), "snapshot.json")
	s := NewStore(LocalSource{Dir: dir}, cache, "test-source")
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := s.Status()
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.Status().LoadedAt.Equal(first.LoadedAt) {
		t.Fatal("unchanged content replaced snapshot")
	}
	writeFile(t, dir, "context/a.md", "---\ntitle: broken")
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("invalid refresh succeeded")
	}
	r, err := s.Read("context/a.md")
	if err != nil || r.Revision != first.Revision || len(r.Warnings) == 0 || r.Document.Content != "# First\nOriginal." {
		t.Fatalf("lost valid content: %+v, %v", r, err)
	}
	restarted := NewStore(LocalSource{Dir: dir}, cache, "test-source")
	if err := restarted.Refresh(context.Background()); err == nil {
		t.Fatal("invalid refresh succeeded")
	}
	if err := restarted.RestoreCache(); err != nil {
		t.Fatal(err)
	}
	if restarted.Status().Revision != first.Revision || restarted.Status().LastError == "" {
		t.Fatal("cache did not retain provenance and degraded status")
	}
	wrong := NewStore(LocalSource{Dir: dir}, cache, "another-source")
	if err := wrong.RestoreCache(); err == nil {
		t.Fatal("cache accepted for wrong source")
	}
	writeFile(t, dir, "context/a.md", "# Updated\nNew content.")
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status().Revision == first.Revision || s.Status().LastError != "" {
		t.Fatal("did not recover")
	}
}

func TestConcurrentReadsAndRefresh(t *testing.T) {
	s := NewStore(fixedSource{fixture()}, "", "")
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if _, err := s.List(Query{Query: "gateway"}); err != nil {
					t.Error(err)
				}
				if _, err := s.Read("context/routing.md"); err != nil {
					t.Error(err)
				}
				_ = s.Status()
			}
		}()
	}
	for range 10 {
		if err := s.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

type fixedSource struct{ c Candidate }

func (f fixedSource) Load(context.Context) (Candidate, error) { return f.c, nil }

func TestGitRefreshAndOfflineCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	writeFile(t, dir, "content/context/a.md", "# First")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "first")
	state := t.TempDir()
	src := GitSource{Remote: dir, Branch: "main", Root: "content", CacheDir: filepath.Join(state, "git")}
	store := NewStore(src, filepath.Join(state, "snapshot.json"), "git-fixture")
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := store.Status().Revision
	writeFile(t, dir, "content/context/a.md", "# Updated")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "second")
	if err := store.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.Status().Revision == first {
		t.Fatal("Git revision did not change")
	}
	second := store.Status().Revision
	// A bad content commit must not replace the previous revision.
	writeFile(t, dir, "content/context/a.md", "---\ntitle: broken")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "invalid")
	if err := store.Refresh(context.Background()); err == nil {
		t.Fatal("invalid Git content accepted")
	}
	if store.Status().Revision != second {
		t.Fatal("bad commit replaced active snapshot")
	}
	if err := os.Rename(dir, dir+"-offline"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir + "-offline") })
	restarted := NewStore(src, filepath.Join(state, "snapshot.json"), "git-fixture")
	if err := restarted.Refresh(context.Background()); err == nil {
		t.Fatal("missing remote should fail")
	}
	if err := restarted.RestoreCache(); err != nil {
		t.Fatal(err)
	}
	r, err := restarted.Read("context/a.md")
	if err != nil || r.Document.Content != "# Updated" || r.Revision != second || len(r.Warnings) == 0 {
		t.Fatalf("offline recovery: %+v %v", r, err)
	}
}

func writeFile(t *testing.T, root, p, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, b, err)
	}
}
