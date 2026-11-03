//go:build unix

package app

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// spawnDetached starts ros-local again in its own session, so it outlives the hook process (Claude Code
// gives SessionEnd hooks about 1.5 s) and isn't killed with the hook's process group.
func spawnDetached(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// execProgram replaces this process with argv[0] run in dir (so Claude Code owns the terminal and exits with
// its own status).
func execProgram(dir string, argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
