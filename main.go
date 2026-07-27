// Command chore runs tasks defined in a chores.yml.
//
// It reads the same file format as go-task, so a project can be driven by either
// while the differences are evaluated — but a task here can take positional
// arguments, and variables resolve once, at invocation, with arguments on top.
package main

import (
	"os"

	"github.com/antimatter-studios/chore/internal/cli"
)

// version is stamped by the release build:
//
//	go build -ldflags "-X main.version=1.2.3"
//
// "dev" when built from source, which is also what the Homebrew formula's test
// asserts against, so it must never be empty.
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
