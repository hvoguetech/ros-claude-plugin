//go:build !unix

package app

import (
	"errors"
	"os"
	"os/exec"
)

// Windows isn't a supported platform yet (spec §18, P4): SessionEnd runs inline instead.
func spawnDetached(args ...string) error {
	return errors.New("detached processes are not supported here")
}

func openBrowser(url string) error { return errors.New("opening a browser is not supported here") }

// execProgram runs argv[0] in dir as a child and waits for it (no exec on this platform).
func execProgram(dir string, argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
