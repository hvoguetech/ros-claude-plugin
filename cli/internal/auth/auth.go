// Package auth owns the device tokens (spec §6): the RFC 8628 sign-in and refreshing the access token
// ahead of expiry. Claude Code never refreshes anything; it calls `ros-local mcp-headers`, which calls
// AccessToken here.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/api"
	"github.com/hvoguetech/ros-claude-plugin/cli/internal/config"
)

// RefreshAhead is how long before expiry the access token is refreshed. The backend's access tokens live
// 60 min, so a token is never sent with less than this left, and `mcp-headers` (10 s budget) almost never
// has to wait on the network.
const RefreshAhead = 10 * time.Minute

// ErrSignedOut means there are no usable credentials: never signed in, or the refresh token was refused
// (expired, device revoked, account disabled). The fix is `ros-local login`.
var ErrSignedOut = errors.New("ROS: signed out — run `ros-local login`")

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

// AccessToken returns a device access token with more than RefreshAhead left, refreshing it when needed.
// If the refresh fails for a transient reason (offline, 5xx) and the current token hasn't expired yet,
// the current token is returned: a slightly stale token beats no token. A refused refresh token returns
// ErrSignedOut.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	creds, err := s.Store.Get(s.Config.URL)
	if errors.Is(err, config.ErrNoCredentials) {
		return "", ErrSignedOut
	}
	if err != nil {
		return "", fmt.Errorf("reading credentials from the %s: %w", s.Store.Name(), err)
	}
	if Valid(creds, RefreshAhead) {
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
		var apiErr *api.Error
		if errors.As(err, &apiErr) && (apiErr.Code == "invalid_grant" || apiErr.Status == 401) {
			return "", ErrSignedOut
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
	if tok.RefreshToken != "" { // rotation, should the backend start doing it
		next.RefreshToken = tok.RefreshToken
	}
	// A failed save still leaves a usable token for this call; the next call just refreshes again.
	_ = s.Store.Set(s.Config.URL, next)
	return next.AccessToken, nil
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
