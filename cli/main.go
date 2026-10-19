// Command ros-local is the helper CLI for the ROS Claude Code plugin: device sign-in, repo linking,
// skills sync, the MCP headersHelper and the SessionStart / SessionEnd hooks.
package main

import (
	"os"

	"github.com/hvoguetech/ros-claude-plugin/cli/internal/app"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	os.Exit(app.New(version).Run(os.Args[1:]))
}
