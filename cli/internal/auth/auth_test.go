package auth

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fakeros"
)

func newSession(t *testing.T, srv *fakeros.Server) *Session {
	t.Helper()
	t.Setenv("ROS_HOME", t.TempDir())
	return &Session{Config: &config.Config{URL: srv.URL}, Store: config.FileStore{},
		NewClient: func(base, token string) *api.Client { return api.New(base, token, "test", 5*time.Second) }}
}

func store(t *testing.T, s *Session, access string, expiresIn time.Duration) {
	t.Helper()
	if err := s.Store.Set(s.Config.URL, &config.Credentials{AccessToken: access,
		AccessExpiresAt: time.Now().Add(expiresIn), RefreshToken: "refresh-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestFreshTokenIsNotRefreshed(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	s := newSession(t, srv)
	store(t, s, "current", 50*time.Minute)
	tok, err := s.AccessToken(context.Background())
	if err != nil || tok != "current" || srv.RefreshCalls != 0 {
		t.Fatalf("tok=%q err=%v refreshes=%d", tok, err, srv.RefreshCalls)
	}
}

func TestRefreshesAheadOfExpiry(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	s := newSession(t, srv)
	store(t, s, "current", RefreshAhead-time.Minute) // still valid, but inside the refresh window
	tok, err := s.AccessToken(context.Background())
	if err != nil || tok == "current" || srv.RefreshCalls != 1 {
		t.Fatalf("tok=%q err=%v refreshes=%d", tok, err, srv.RefreshCalls)
	}
	saved, _ := s.Store.Get(s.Config.URL)
	if saved.AccessToken != tok || saved.RefreshToken != "refresh-1" || time.Until(saved.AccessExpiresAt) < 50*time.Minute {
		t.Fatalf("refreshed token not stored correctly: %+v", saved)
	}
	// The next call uses the stored token without another refresh.
	if tok2, _ := s.AccessToken(context.Background()); tok2 != tok || srv.RefreshCalls != 1 {
		t.Fatalf("second call refreshed again")
	}
}

func TestTransientRefreshFailureKeepsValidToken(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.RefreshError = "server_error"
	s := newSession(t, srv)
	store(t, s, "current", 2*time.Minute)
	tok, err := s.AccessToken(context.Background())
	if err != nil || tok != "current" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	store(t, s, "expired", -time.Minute)
	if _, err := s.AccessToken(context.Background()); err == nil || errors.Is(err, ErrSignedOut) {
		t.Fatalf("expired token + 5xx refresh should be a plain error, got %v", err)
	}
}

func TestRefusedRefreshIsSignedOut(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.RefreshError = "invalid_grant"
	s := newSession(t, srv)
	store(t, s, "current", time.Minute)
	if _, err := s.AccessToken(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("want ErrSignedOut, got %v", err)
	}
}

func TestNoCredentialsIsSignedOut(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	s := newSession(t, srv)
	if _, err := s.AccessToken(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("want ErrSignedOut, got %v", err)
	}
}

func TestLoginDeviceFlow(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.SlowDownPolls, srv.PendingPolls = 1, 2
	s := newSession(t, srv)
	var waits []time.Duration
	tok, err := s.Login(context.Background(), LoginOptions{Out: io.Discard, DeviceName: "laptop",
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if tok.DeviceID != "dev1" || tok.ProjectID != "p1" {
		t.Fatalf("%+v", tok)
	}
	// interval 5s, +5s after slow_down (RFC 8628 §3.5)
	if len(waits) != 4 || waits[0] != 5*time.Second || waits[1] != 10*time.Second {
		t.Fatalf("poll waits %v", waits)
	}
	creds, err := s.Store.Get(s.Config.URL)
	if err != nil || creds.RefreshToken != "refresh-1" || creds.AccessToken != tok.AccessToken {
		t.Fatalf("creds %+v err %v", creds, err)
	}
}

func TestLoginDenied(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.DenyLogin = true
	s := newSession(t, srv)
	_, err := s.Login(context.Background(), LoginOptions{Out: io.Discard,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	if err == nil || err.Error() != "sign-in was denied" {
		t.Fatalf("got %v", err)
	}
}

func TestRotatedRefreshTokenIsStored(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.RotateRefresh = true
	s := newSession(t, srv)
	store(t, s, "old", time.Minute)
	if _, err := s.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.Get(s.Config.URL)
	if saved.RefreshToken == "refresh-1" || saved.RefreshToken == "" {
		t.Fatalf("rotated refresh token not stored: %+v", saved)
	}
	// Expire again: the next refresh uses the rotated token, so no reuse is detected.
	saved.AccessExpiresAt = time.Now().Add(time.Minute)
	_ = s.Store.Set(s.Config.URL, saved)
	if _, err := s.AccessToken(context.Background()); err != nil || srv.ReuseDetected {
		t.Fatalf("second refresh: err=%v reuse=%v", err, srv.ReuseDetected)
	}
}

func TestReusedRefreshTokenSignsOutAndClearsCredentials(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.RotateRefresh = true
	s := newSession(t, srv)
	store(t, s, "old", time.Minute)
	if _, err := s.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Put the spent token back, as a clobbering writer would.
	store(t, s, "old", time.Minute)
	if _, err := s.AccessToken(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("want ErrSignedOut on reuse, got %v", err)
	}
	if _, err := s.Store.Get(s.Config.URL); !errors.Is(err, config.ErrNoCredentials) {
		t.Fatalf("dead credentials not removed: %v", err)
	}
}

// Hooks and the MCP headersHelper start together. With rotation, two refreshes with the same token
// would be reuse and revoke the device; the lock makes the others wait and reuse the first refresh.
func TestConcurrentRefreshesDoNotTripReuseDetection(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.RotateRefresh = true
	srv.RefreshDelay = 100 * time.Millisecond
	s := newSession(t, srv)
	store(t, s, "old", time.Minute)
	const n = 6
	toks := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			sess := &Session{Config: s.Config, Store: s.Store, NewClient: s.NewClient}
			tok, err := sess.AccessToken(context.Background())
			toks <- tok
			errs <- err
		}()
	}
	first := ""
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
		tok := <-toks
		if first == "" {
			first = tok
		} else if tok != first {
			t.Fatalf("callers got different tokens: %q vs %q", first, tok)
		}
	}
	if srv.ReuseDetected || srv.RefreshCalls != 1 {
		t.Fatalf("reuse=%v refreshes=%d, want one refresh and no reuse", srv.ReuseDetected, srv.RefreshCalls)
	}
}

func TestLoginLocalAgentDisabled(t *testing.T) {
	srv := fakeros.New()
	defer srv.Close()
	srv.LocalAgentDisabled = true
	s := newSession(t, srv)
	_, err := s.Login(context.Background(), LoginOptions{Out: io.Discard,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	if err == nil || err.Error() != LocalAgentDisabledMsg {
		t.Fatalf("got %v", err)
	}
}
