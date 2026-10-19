//go:build !unix

package app

import "errors"

// Windows isn't a supported platform yet (spec §18, P4): SessionEnd runs inline instead.
func spawnDetached(args ...string) error {
	return errors.New("detached processes are not supported here")
}

func openBrowser(url string) error { return errors.New("opening a browser is not supported here") }
