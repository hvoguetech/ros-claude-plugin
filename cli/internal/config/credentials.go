package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/fsutil"
	"github.com/zalando/go-keyring"
)

// ErrNoCredentials means the user isn't signed in.
var ErrNoCredentials = errors.New("not signed in")

// keyringService is the keychain item's service name; the account is the ROS URL.
const keyringService = "ros-local"

// Credentials are the device tokens (spec §6). They live only in the credential store, never in env or
// the config file.
type Credentials struct {
	AccessToken     string    `json:"access_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	RefreshToken    string    `json:"refresh_token"`
}

// Store keeps one set of credentials per ROS URL.
type Store interface {
	Name() string
	Get(url string) (*Credentials, error) // ErrNoCredentials when absent
	Set(url string, c *Credentials) error
	Delete(url string) error
}

// StoreFor returns the store the config names. $ROS_STORE (keychain | file) overrides it, for tests and
// headless machines.
func StoreFor(c *Config) Store {
	name := c.Store
	if env := os.Getenv("ROS_STORE"); env != "" {
		name = env
	}
	if name == "file" {
		return FileStore{}
	}
	return KeychainStore{}
}

// KeychainStore uses the macOS Keychain or the Linux Secret Service (via D-Bus).
type KeychainStore struct{}

func (KeychainStore) Name() string { return "keychain" }

func (KeychainStore) Get(url string) (*Credentials, error) {
	s, err := keyring.Get(keyringService, url)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNoCredentials
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (KeychainStore) Set(url string, c *Credentials) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return keyring.Set(keyringService, url, string(b))
}

func (KeychainStore) Delete(url string) error {
	err := keyring.Delete(keyringService, url)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// Available reports whether the keychain answers at all (a Linux box without a Secret Service doesn't).
func (KeychainStore) Available() bool {
	_, err := keyring.Get(keyringService, "__probe__")
	return err == nil || errors.Is(err, keyring.ErrNotFound)
}

// FileStore is the fallback: `~/.ros/credentials.json`, mode 0600.
type FileStore struct{}

func (FileStore) Name() string { return "file" }

func filePath() string { return filepath.Join(Home(), "credentials.json") }

func (FileStore) all() (map[string]*Credentials, error) {
	m := map[string]*Credentials{}
	b, err := os.ReadFile(filePath())
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f FileStore) write(m map[string]*Credentials) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filePath(), b, 0o600)
}

func (f FileStore) Get(url string) (*Credentials, error) {
	m, err := f.all()
	if err != nil {
		return nil, err
	}
	if c, ok := m[url]; ok && c != nil {
		return c, nil
	}
	return nil, ErrNoCredentials
}

func (f FileStore) Set(url string, c *Credentials) error {
	m, err := f.all()
	if err != nil {
		return err
	}
	m[url] = c
	return f.write(m)
}

func (f FileStore) Delete(url string) error {
	m, err := f.all()
	if err != nil {
		return err
	}
	if _, ok := m[url]; !ok {
		return nil
	}
	delete(m, url)
	if len(m) == 0 {
		err := os.Remove(filePath())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return f.write(m)
}
