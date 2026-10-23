// Package auth owns the device tokens (spec §6): the RFC 8628 sign-in and refreshing the access token
// ahead of expiry. Claude Code never refreshes anything; it calls `ros-local mcp-headers`, which calls
// AccessToken here.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
)

// RefreshAhead is how long before expiry the access token is refreshed. The backend's access tokens live
// 60 min, so a token is never sent with less than this left, and `mcp-headers` (10 s budget) almost never
// has to wait on the network.
const RefreshAhead = 10 * time.Minute

// ErrSignedOut means there are no usable credentials: never signed in, or the refresh token was refused
// (expired, device revoked, account disabled). The fix is `ros-local login`.
var ErrSignedOut = errors.New("ROS: signed out — run `ros-local login`")

// LocalAgentDisabledMsg is shown wherever the server answers local_agent_disabled.
const LocalAgentDisabledMsg = "Local agent isn't enabled for your org/project"

// ErrLocalAgentDisabled: the server refused a refresh because the local agent is off. The credentials
// are kept; it is not a sign-out.
var ErrLocalAgentDisabled = errors.New(LocalAgentDisabledMsg)

// Now is the clock (tests replace it).
var Now = time.Now

// Session is the signed-in state for one ROS URL.
type Session struct {
	Config *config.Config
	Store  config.Store
	// NewClient builds the API client (tests point it at a fake server).
	NewClient func(baseURL, token string) *api.Client
}

// Valid reports whether the access token is usable for at least d more.
func Valid(c *config.Credentials, d time.Duration) bool {
	return c != nil && c.AccessToken != "" && Now().Add(d).Before(c.AccessExpiresAt)
}

// LockPath is the file that serialises refreshes across ros-local processes (hooks and the MCP
// headersHelper start at the same moment). With refresh-token rotation, two processes refreshing with
// the same token would look like reuse to the server, which revokes the device.
func LockPath() string { return filepath.Join(config.Home(), "refresh.lock") }

// refused reports whether the server refused the refresh token for good. Per the backend contract
// that is exactly `invalid_grant`: expired, the device revoked, or a rotated token presented after its
// grace window (reuse detection). The only fix is `ros-local login`. Anything else (5xx, offline,
// local_agent_disabled) leaves the refresh token in place.
func refused(err error) bool {
	return api.OAuthCode(err) == "invalid_grant"
}

// AccessToken returns a device access token with more than RefreshAhead left, refreshing it when needed.
// Refreshes are serialised by a file lock, and the credentials are re-read once it's held, so a refresh
// another process just did is reused instead of repeated. The rotated refresh token is saved before the
// lock is released. If the refresh fails for a transient reason (offline, 5xx) and the current token
// hasn't expired yet, the current token is returned: a slightly stale token beats no token. A refused
// refresh token returns ErrSignedOut and the dead credentials are removed.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	creds, err := s.read()
	if err != nil {
		return "", err
	}
	if Valid(creds, RefreshAhead) {
		return creds.AccessToken, nil
	}
	if unlock, lerr := fsutil.Lock(ctx, LockPath()); lerr == nil {
		defer unlock()
		if creds, err = s.read(); err != nil {
			return "", err
		}
		if Valid(creds, RefreshAhead) { // another process refreshed while we waited
			return creds.AccessToken, nil
		}
	} else if Valid(creds, 0) {
		// Couldn't get the lock in time (another refresh is slow): use the still-valid token rather
		// than racing that refresh with the same refresh token.
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		if Valid(creds, 0) {
			return creds.AccessToken, nil
		}
		return "", ErrSignedOut
	}
	tok, err := s.NewClient(s.Config.URL, "").Refresh(ctx, config.ClientID, creds.RefreshToken)
	if err != nil {
		if refused(err) {
			_ = s.Store.Delete(s.Config.URL)
			return "", ErrSignedOut
		}
		if api.HasCode(err, api.CodeLocalAgentDisabled) {
			// Nothing was rotated: keep the refresh token for when an admin turns the local agent on.
			return "", ErrLocalAgentDisabled
		}
		if Valid(creds, 0) {
			return creds.AccessToken, nil
		}
		return "", fmt.Errorf("refreshing the ROS token: %w", err)
	}
	next := &config.Credentials{
		AccessToken:     tok.AccessToken,
		AccessExpiresAt: Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
		RefreshToken:    creds.RefreshToken,
	}
	if tok.RefreshToken != "" { // rotated: the old refresh token is now spent
		next.RefreshToken = tok.RefreshToken
	}
	// The rotated token must be saved: the spent one would trip reuse detection on the next refresh.
	// Retry once; if the store still fails, this call still gets its access token.
	if err := s.Store.Set(s.Config.URL, next); err != nil {
		_ = s.Store.Set(s.Config.URL, next)
	}
	return next.AccessToken, nil
}

func (s *Session) read() (*config.Credentials, error) {
	creds, err := s.Store.Get(s.Config.URL)
	if errors.Is(err, config.ErrNoCredentials) {
		return nil, ErrSignedOut
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials from the %s: %w", s.Store.Name(), err)
	}
	return creds, nil
}

// LoginOptions configure the device flow.
type LoginOptions struct {
	ProjectID  string
	DeviceName string
	Platform   string
	Version    string
	Out        io.Writer
	// OpenBrowser is called with the verification URL; nil means print only.
	OpenBrowser func(url string) error
	// Sleep waits between polls (tests replace it).
	Sleep func(ctx context.Context, d time.Duration) error
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Login runs the RFC 8628 device flow and stores the tokens. It returns the token answer (device id,
// approved project) so the caller can finish setting up.
func (s *Session) Login(ctx context.Context, o LoginOptions) (*api.Token, error) {
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	c := s.NewClient(s.Config.URL, "")
	da, err := c.StartDeviceAuthorization(ctx, config.ClientID, api.DeviceRequest{
		ProjectID: o.ProjectID, DeviceName: o.DeviceName, Platform: o.Platform, PluginVersion: o.Version})
	if err != nil {
		if api.HasCode(err, api.CodeLocalAgentDisabled) {
			return nil, errors.New(LocalAgentDisabledMsg)
		}
		if api.IsStatus(err, 404) {
			return nil, fmt.Errorf("local agent sign-in is not enabled on %s", s.Config.URL)
		}
		return nil, fmt.Errorf("starting sign-in: %w", err)
	}
	link := da.VerificationURIComplete
	if link == "" {
		link = da.VerificationURI
	}
	fmt.Fprintf(o.Out, "To sign in, open %s\nand confirm the code %s\n\n", link, da.UserCode)
	if o.OpenBrowser != nil {
		_ = o.OpenBrowser(link)
	}
	fmt.Fprintln(o.Out, "Waiting for approval…")

	interval := time.Duration(max(da.Interval, 1)) * time.Second
	deadline := Now().Add(time.Duration(max(da.ExpiresIn, 60)) * time.Second)
	for {
		if err := o.Sleep(ctx, interval); err != nil {
			return nil, err
		}
		tok, err := c.ExchangeDeviceCode(ctx, config.ClientID, da.DeviceCode)
		if err == nil {
			creds := &config.Credentials{
				AccessToken:     tok.AccessToken,
				AccessExpiresAt: Now().Add(time.Duration(tok.ExpiresIn) * time.Second),
				RefreshToken:    tok.RefreshToken,
			}
			if err := s.Store.Set(s.Config.URL, creds); err != nil {
				return nil, fmt.Errorf("saving credentials to the %s: %w", s.Store.Name(), err)
			}
			return tok, nil
		}
		switch api.OAuthCode(err) {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second // RFC 8628 §3.5
		case "access_denied":
			return nil, errors.New("sign-in was denied")
		case "expired_token":
			return nil, errors.New("the code expired before it was approved; run `ros-local login` again")
		default:
			return nil, fmt.Errorf("signing in: %w", err)
		}
		if Now().After(deadline) {
			return nil, errors.New("the code expired before it was approved; run `ros-local login` again")
		}
	}
}
