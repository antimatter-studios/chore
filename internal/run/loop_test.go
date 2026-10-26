package run

import (
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

// loopFixture decodes a whole Taskfile, so these tests exercise `for:` from the
// YAML a person writes rather than from structs nobody would.
func loopFixture(t *testing.T, yaml string) *fixture {
	t.Helper()
	file, err := chorefile.Decode([]byte(yaml))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return newFixture(t, file, file.Tasks)
}

func TestForOverAListVariable(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  DRIVERS: [ext4, ntfs, xfs]
tasks:
  all:
    cmds:
      - for: {var: DRIVERS}
        cmd: echo "{{.ITEM}} $ITEM" >> out.txt
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "ext4 ext4\nntfs ntfs\nxfs xfs\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

func TestForOverAMapCallsATaskPerEntry(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    vars:
      PORTS: {map: {worker: 9090, api: 8080, web: 80}}
    cmds:
      - for: {var: PORTS}
        task: open
        vars: {NAME: '{{.KEY}}', PORT: '{{.ITEM}}'}
  open:
    requires: [NAME, PORT]
    cmds:
      - echo "{{.NAME}}={{.PORT}}" >> out.txt
`)
	f.mustRun("all", nil, nil)
	// Written order, not sorted: worker before api.
	if got, want := f.read("out.txt"), "worker=9090\napi=8080\nweb=80\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

func TestForAs(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  ENV: prod
tasks:
  all:
    cmds:
      - for: ['{{.ENV}}-a', '{{.ENV}}-b']
        cmd: echo {{.ITEM}} >> out.txt
      - for: {var: HOSTS, as: HOST}
        cmd: echo "{{.HOST}} $HOST" >> hosts.txt
    vars:
      HOSTS: [h1, h2]
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "prod-a\nprod-b\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
	if got, want := f.read("hosts.txt"), "h1 h1\nh2 h2\n"; got != want {
		t.Errorf("hosts.txt = %q, want %q", got, want)
	}
}

func TestForSplitsAString(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  SPACED: "  a  b
    c "
  CSV: "x, y,,z"
  FILES: {sh: printf 'one\ntwo\n'}
tasks:
  all:
    cmds:
      - for: {var: SPACED}
        cmd: echo {{.ITEM}} >> spaced.txt
      - for: {var: CSV, split: ','}
        cmd: echo "[{{.ITEM}}]" >> csv.txt
      - for: {var: FILES}
        cmd: echo {{.ITEM}} >> files.txt
`)
	f.mustRun("all", nil, nil)
	for file, want := range map[string]string{
		"spaced.txt": "a\nb\nc\n",
		"csv.txt":    "[x]\n[y]\n[z]\n",
		"files.txt":  "one\ntwo\n",
	} {
		if got := f.read(file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
}

// A value typed on the command line replaces the file's list, and the loop
// splits it.
func TestForACommandLineValueReplacesAList(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  DRIVERS: [ext4, ntfs]
tasks:
  all:
    cmds:
      - for: {var: DRIVERS}
        cmd: echo {{.ITEM}} >> out.txt
`)
	f.r.CLIVars = map[string]string{"DRIVERS": "xfs btrfs"}
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "xfs\nbtrfs\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

// A list passed to another task arrives as its string form, and a loop there
// splits it back into the same items.
func TestForAListSurvivesBeingPassedOn(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  DRIVERS: [ext4, ntfs]
tasks:
  all:
    cmds:
      - task: each
        vars: {LIST: '{{.DRIVERS}}'}
  each:
    cmds:
      - for: {var: LIST}
        cmd: echo {{.ITEM}} >> out.txt
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "ext4\nntfs\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

func TestForOverAnUnsetVariableIsAnError(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: {var: DRIVRES}
        cmd: echo {{.ITEM}} >> out.txt
`)
	err := f.mustFail("all", nil, nil)
	mustContain(t, err.Error(), "variable DRIVRES is not set", "error")
	if f.exists("out.txt") {
		t.Error("the loop body ran")
	}
}

func TestForOverAnEmptyListRunsNothing(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  NONE: []
  BLANK: ""
tasks:
  all:
    cmds:
      - for: {var: NONE}
        cmd: echo {{.ITEM}} >> out.txt
      - for: {var: BLANK}
        cmd: echo {{.ITEM}} >> out.txt
      - echo done >> done.txt
`)
	f.mustRun("all", nil, nil)
	if f.exists("out.txt") {
		t.Errorf("an empty loop ran its body: %q", f.read("out.txt"))
	}
	if !f.exists("done.txt") {
		t.Error("the step after an empty loop did not run")
	}
}

// The first failing item stops the loop and fails the task, as a failing step in
// `cmds:` does.
func TestForStopsAtTheFirstFailure(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: [a, b, c]
        cmd: echo {{.ITEM}} >> out.txt && test {{.ITEM}} != b
      - echo after >> after.txt
`)
	f.mustFail("all", nil, nil)
	if got, want := f.read("out.txt"), "a\nb\n"; got != want {
		t.Errorf("out.txt = %q, want %q — c must not run", got, want)
	}
	if f.exists("after.txt") {
		t.Error("the step after a failed loop ran")
	}
}

// With ignore_error a failing item is reported, and the loop goes on.
func TestForIgnoreErrorCarriesOn(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: [a, b, c]
        cmd: echo {{.ITEM}} >> out.txt && test {{.ITEM}} != b
        ignore_error: true
      - echo after >> after.txt
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "a\nb\nc\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
	if !f.exists("after.txt") {
		t.Error("the step after the loop did not run")
	}
	mustContain(t, f.err.String(), `for ITEM="b" failed, ignored`, "stderr")
}

func TestForIgnoreErrorNamesTheMapKey(t *testing.T) {
	f := loopFixture(t, `version: '3'
vars:
  M: {map: {good: 0, bad: 1}}
tasks:
  all:
    cmds:
      - for: {var: M}
        cmd: exit {{.ITEM}}
        ignore_error: true
`)
	f.mustRun("all", nil, nil)
	mustContain(t, f.err.String(), `for KEY="bad" failed, ignored`, "stderr")
}

// A deferred loop runs at the end of the task, its items in order.
func TestForInsideADefer(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - defer: {for: [x, y], cmd: 'echo down {{.ITEM}} >> out.txt'}
      - echo up >> out.txt
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "up\ndown x\ndown y\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

// `run: once` keys on rendered vars, so each distinct item runs once and a
// repeated one does not run again.
func TestForWithRunOnce(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: [a, b, a]
        task: build
        vars: {D: '{{.ITEM}}'}
  build:
    run: once
    cmds:
      - echo {{.D}} >> out.txt
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "a\nb\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

// The task name may be templated on the item.
func TestForTemplatedTaskName(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: [a, b]
        task: 'build:{{.ITEM}}'
  build:a:
    cmds: [echo a >> out.txt]
  build:b:
    cmds: [echo b >> out.txt]
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "a\nb\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

// --dry prints each iteration's command instead of running it.
func TestForDryRun(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: [a, b]
        cmd: echo {{.ITEM}} >> out.txt
`)
	f.r.DryRun = true
	f.mustRun("all", nil, nil)
	if f.exists("out.txt") {
		t.Error("--dry ran the loop")
	}
	if got := f.out.String(); !strings.Contains(got, "echo a >> out.txt\necho b >> out.txt\n") {
		t.Errorf("stdout = %q, want both rendered commands", got)
	}
}

// A loop inside a hook loops the same way.
func TestForInsideAHook(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    after:
      - for: [x, y]
        cmd: echo after {{.ITEM}} >> out.txt
    cmds:
      - echo body >> out.txt
`)
	f.mustRun("all", nil, nil)
	if got, want := f.read("out.txt"), "body\nafter x\nafter y\n"; got != want {
		t.Errorf("out.txt = %q, want %q", got, want)
	}
}

func TestForRenderErrorIsReported(t *testing.T) {
	f := loopFixture(t, `version: '3'
tasks:
  all:
    cmds:
      - for: ['{{.X']
        cmd: echo {{.ITEM}}
`)
	err := f.mustFail("all", nil, nil)
	mustContain(t, err.Error(), "for:", "error")
}
