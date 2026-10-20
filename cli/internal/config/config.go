// Package config holds ros-local's non-secret settings (`~/.ros/config.json`) and its credential store
// (the OS keychain, or a 0600 file where no keychain is available).
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
)

// DefaultURL is the production ROS API. Self-hosted and staging customers pass `ros-local login --url`.
const DefaultURL = "https://api.agentstudio.revora.dev"

// ClientID identifies ros-local to the OAuth server (a public client, RFC 8628).
const ClientID = "ros-local"

// Config is `~/.ros/config.json`. It holds nothing secret.
type Config struct {
	URL              string    `json:"url"`
	Store            string    `json:"credential_store,omitempty"` // keychain | file
	DeviceID         string    `json:"device_id,omitempty"`
	DefaultProjectID string    `json:"default_project_id,omitempty"`
	UserEmail        string    `json:"user_email,omitempty"`
	Scopes           []string  `json:"scopes,omitempty"`
	SignedInAt       time.Time `json:"signed_in_at,omitzero"`
}

// Home is ros-local's state directory: $ROS_HOME, else ~/.ros.
func Home() string {
	if h := os.Getenv("ROS_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".ros")
	}
	return ".ros"
}

func path() string { return filepath.Join(Home(), "config.json") }

// Load reads the config. A missing file yields defaults.
func Load() (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, err
		}
	}
	if c.URL == "" {
		c.URL = DefaultURL
	}
	c.URL = NormalizeURL(c.URL)
	return c, nil
}

// Save writes the config.
func (c *Config) Save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path(), append(b, '\n'), 0o600)
}

// NormalizeURL trims whitespace and trailing slashes.
func NormalizeURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}
