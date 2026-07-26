// Command tsk runs tasks defined in a Taskfile.yml.
//
// It reads the same file format as go-task, so a project can be driven by either
// while the differences are evaluated — but a task here can take positional
// arguments, and variables resolve once, at invocation, with arguments on top.
package main

import (
	"os"

	"github.com/rest-mail/go-tsk/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
