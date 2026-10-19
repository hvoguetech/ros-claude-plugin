package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// Cases mirror the backend's repo_key (ros/services/local_config.py).
func TestRepoKey(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Acme/Payments-API.git":             "github.com/acme/payments-api",
		"https://github.com/acme/payments-api.git":         "github.com/acme/payments-api",
		"https://user:pw@github.com:443/acme/payments-api": "github.com/acme/payments-api",
		"ssh://git@github.com/acme/payments-api.git/":      "github.com/acme/payments-api",
		"https://gitlab.example.com/group/sub/repo.git":    "gitlab.example.com/group/sub/repo",
		"github.com:acme/payments-api":                     "github.com/acme/payments-api",
		"":                                                 "",
		"not a remote":                                     "",
		"https://github.com/onlyowner":                     "",
	}
	for in, want := range cases {
		if got := RepoKey(in); got != want {
			t.Errorf("RepoKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	sub := filepath.Join(root, "a", "b")
	_ = os.MkdirAll(sub, 0o755)
	r, err := Find(sub)
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != root || r.GitDir != filepath.Join(root, ".git") || r.CommonDir != r.GitDir {
		t.Fatalf("%+v", r)
	}
	if _, err := Find(t.TempDir()); err != ErrNotRepo {
		t.Fatalf("want ErrNotRepo, got %v", err)
	}
}
