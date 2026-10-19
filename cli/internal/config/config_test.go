package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreIsPrivateAndPerURL(t *testing.T) {
	t.Setenv("ROS_HOME", t.TempDir())
	s := FileStore{}
	if _, err := s.Get("https://a"); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty store: %v", err)
	}
	a := &Credentials{AccessToken: "ta", RefreshToken: "ra", AccessExpiresAt: time.Now()}
	b := &Credentials{AccessToken: "tb", RefreshToken: "rb", AccessExpiresAt: time.Now()}
	if err := s.Set("https://a", a); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("https://b", b); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(Home(), "credentials.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode %v err %v", fi.Mode().Perm(), err)
	}
	if got, _ := s.Get("https://a"); got.RefreshToken != "ra" {
		t.Fatalf("%+v", got)
	}
	_ = s.Delete("https://a")
	if _, err := s.Get("https://a"); !errors.Is(err, ErrNoCredentials) {
		t.Fatal("not deleted")
	}
	if got, _ := s.Get("https://b"); got == nil || got.RefreshToken != "rb" {
		t.Fatal("deleting one URL dropped another")
	}
}

func TestLoadDefaultsAndSaveIsPrivate(t *testing.T) {
	t.Setenv("ROS_HOME", t.TempDir())
	c, err := Load()
	if err != nil || c.URL != DefaultURL {
		t.Fatalf("%+v %v", c, err)
	}
	c.URL = "https://ros.example.com/"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, _ := Load()
	if c2.URL != "https://ros.example.com" {
		t.Fatalf("url %q", c2.URL)
	}
	fi, _ := os.Stat(filepath.Join(Home(), "config.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode().Perm())
	}
}
