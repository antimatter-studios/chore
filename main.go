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

// buildDate is stamped by the release build with the COMMIT's date:
//
//	go build -ldflags "-X main.buildDate=2026-07-28T11:21:58Z"
//
// Empty when built from source, where the toolchain records vcs.time instead. It is
// never the wall clock: the release must rebuild to identical bytes.
var buildDate = ""

func main() {
	cli.Version = version
	cli.BuildDate = buildDate
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
