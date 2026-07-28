package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func groups() []Group {
	return []Group{
		{Name: "instance", Tasks: []Task{
			{Name: "instance:up", Desc: "Bring up the full stack"},
			{Name: "instance:down", Desc: "Stop it"},
		}},
		{Name: "(root)", Tasks: []Task{
			{Name: "build", Desc: "Build all Go binaries"},
			{Name: "bare"},
		}},
	}
}

// styled builds a UI that renders as if it were attached to a colour terminal.
// New() correctly refuses to style a bytes.Buffer, which is the whole point of
// it, so a test about styling has to ask for a profile explicitly.
func styled(w *bytes.Buffer) *UI { return newWith(w, termenv.TrueColor) }

// TestPlainIsUnchanged is the contract that lets this package exist at all: a
// pipe gets the bytes chore has always written. chore's own cli tests parse the
// listing line by line, and rest-mail's e2e workflow runs chore in CI.
func TestPlainIsUnchanged(t *testing.T) {
	var b bytes.Buffer
	New(&b).List(groups())

	// Groups sort by name, tasks within a group by name, and the name column is
	// padded to the widest name across ALL groups — "instance:down" here — so
	// descriptions line up down the whole listing.
	want := strings.Join([]string{
		"tasks:",
		"",
		"  [(root)]",
		"    bare",
		"    build          Build all Go binaries",
		"",
		"  [instance]",
		"    instance:down  Stop it",
		"    instance:up    Bring up the full stack",
		"",
	}, "\n")

	if got := b.String(); got != want {
		t.Errorf("plain listing changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(b.String(), "\x1b[") {
		t.Errorf("plain listing contains ANSI escapes:\n%q", b.String())
	}
}

// TestNonTerminalIsPlain pins the detection itself. A bytes.Buffer is not a
// terminal, so nothing may be styled — this is what keeps CI logs readable
// without every caller having to remember to ask.
func TestNonTerminalIsPlain(t *testing.T) {
	var b bytes.Buffer
	if u := New(&b); !u.Plain() {
		t.Fatal("New(bytes.Buffer) styles output; a pipe must be plain")
	}
}

func TestSetPlainOverrides(t *testing.T) {
	var b bytes.Buffer
	u := styled(&b)
	u.SetPlain(true)
	u.Errorf("something broke")
	if got := b.String(); got != "chore: something broke\n" {
		t.Errorf("SetPlain(true) still styled: %q", got)
	}
}

// TestStyledListHasNoBoxes: the listing is meant to be scanned. A framed table
// per namespace turns a 191-task project into thirty boxes of chrome.
func TestStyledListHasNoBoxes(t *testing.T) {
	var b bytes.Buffer
	styled(&b).List(groups())
	got := b.String()
	if !strings.Contains(got, "\x1b[") {
		t.Error("styled listing has no ANSI escapes")
	}
	for _, box := range []string{"╭", "╰", "│", "┼"} {
		if strings.Contains(got, box) {
			t.Errorf("styled listing draws box borders (%q)", box)
		}
	}
	if !strings.Contains(got, "─") {
		t.Error("styled listing has no rule under a group heading")
	}
}

// TestWideRunesAlign is the bug this replaces: the old listing padded with
// %-*s, which counts BYTES, so one CJK name pushed every later description out
// of column. Widths here are display cells.
func TestWideRunesAlign(t *testing.T) {
	var b bytes.Buffer
	New(&b).List([]Group{{Name: "g", Tasks: []Task{
		{Name: "构建", Desc: "wide"},      // 2 runes, 6 bytes, 4 cells
		{Name: "build", Desc: "narrow"}, // 5 cells
	}}})

	var descCols []int
	for _, line := range strings.Split(b.String(), "\n") {
		i := strings.Index(line, "wide")
		if i < 0 {
			i = strings.Index(line, "narrow")
		}
		if i < 0 || !strings.HasPrefix(line, "    ") {
			continue
		}
		descCols = append(descCols, lipgloss.Width(line[:i]))
	}
	if len(descCols) != 2 {
		t.Fatalf("expected two task lines, got %d\n%s", len(descCols), b.String())
	}
	if descCols[0] != descCols[1] {
		t.Errorf("descriptions start at different columns: %v (a byte-counted pad)", descCols)
	}
}

func TestTruncateKeepsWideRunesWhole(t *testing.T) {
	// Cutting mid-character would emit a broken rune; the ellipsis takes a cell.
	if got := truncate("构建构建", 5); lipgloss.Width(got) > 5 {
		t.Errorf("truncate(%q) = %q, width %d > 5", "构建构建", got, lipgloss.Width(got))
	}
	if got := truncate("short", 40); got != "short" {
		t.Errorf("truncate widened a short string: %q", got)
	}
}

func TestEmptyListing(t *testing.T) {
	var b bytes.Buffer
	New(&b).List(nil)
	if got := b.String(); got != "no tasks\n" {
		t.Errorf("empty listing = %q, want %q", got, "no tasks\n")
	}
}

func TestStyledErrorKeepsTheMessage(t *testing.T) {
	var b bytes.Buffer
	styled(&b).Errorf("no chores.yml here or in any parent directory")
	got := b.String()
	if !strings.Contains(got, "chore:") || !strings.Contains(got, "no chores.yml here") {
		t.Errorf("styled error lost its text: %q", got)
	}
}

// TestColumnsSetsWidth: a caller that forces colour into a capture has no fd to
// measure, so COLUMNS is the only way it can say how wide the output should be.
func TestColumnsSetsWidth(t *testing.T) {
	t.Setenv("COLUMNS", "200")
	var b bytes.Buffer
	u := styled(&b)
	if u.width != 200 {
		t.Fatalf("width = %d, want 200 from COLUMNS", u.width)
	}
	long := strings.Repeat("x", 120)
	u.List([]Group{{Name: "g", Tasks: []Task{{Name: "t", Desc: long}}}})
	if !strings.Contains(b.String(), long) {
		t.Error("a 120-cell description was truncated at a 200-column width")
	}
}

func TestBadColumnsIgnored(t *testing.T) {
	t.Setenv("COLUMNS", "not-a-number")
	var b bytes.Buffer
	if u := styled(&b); u.width != 80 {
		t.Errorf("width = %d, want the 80 default when COLUMNS is junk", u.width)
	}
}
