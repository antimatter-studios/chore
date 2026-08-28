// Package ui renders CHORE'S OWN output — the task listing, headings, errors.
//
// It never touches a TASK's output. internal/shell hands a child process the
// same stdout/stderr chore was given, so what a command prints arrives verbatim:
// interleaving, its own colours, and any progress bar that wants a terminal. That
// is why this package styles strings and returns them rather than owning the
// screen — an event loop would have to capture every byte a task writes and
// re-render it, and `docker compose up` would come out worse for the trouble.
//
// Two rules make the styling safe to add to a program CI depends on:
//
//   - When the destination is not a terminal — a pipe, a file, a CI log — output
//     falls back to EXACTLY the plain text it always was. Not "the same but
//     without colour": the same bytes. rest-mail's e2e workflow parses some of
//     this, and chore's own tests read the listing line by line.
//   - Width is measured in DISPLAY CELLS, not bytes. The listing used to pad with
//     `%-*s`, which counts bytes, so one CJK or emoji character in a task name
//     skewed every following column. lipgloss measures what the terminal shows.
package ui

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// Task and Group are what a listing is made of. Deliberately not chorefile
// types: the renderer has no business knowing what a task can do, and this keeps
// the tests here free of YAML.
type Task struct{ Name, Desc string }

// Group is one namespace's tasks — "instance", "testbed", or "(root)".
type Group struct {
	Name  string
	Tasks []Task
}

// UI renders to one destination. Build one per writer: stdout can be a pipe
// while stderr is still a terminal, and each has to answer for itself.
type UI struct {
	w     io.Writer
	plain bool
	width int

	title   lipgloss.Style
	group   lipgloss.Style
	name    lipgloss.Style
	desc    lipgloss.Style
	border  lipgloss.Style
	errWord lipgloss.Style
	dim     lipgloss.Style
}

// New inspects the writer and returns a UI that styles only if it should.
//
// termenv answers the whole question in one place: it reports an Ascii profile
// for a non-terminal, a dumb terminal, or NO_COLOR being set — the convention
// every CLI is expected to honour. Deciding this once, here, is why no rendering
// path below has to ask "am I allowed to colour this".
func New(w io.Writer) *UI {
	return newWith(w, termenv.NewOutput(w).Profile)
}

// newWith builds a UI for an explicit colour profile.
//
// Styles come from a renderer bound to THIS writer, not from lipgloss's package
// global. The global detects its profile from os.Stdout once, so a style built
// from it renders escapes according to the process's stdout no matter where the
// string is actually going — under `go test` that silently strips every colour,
// and in a program writing to two destinations it would answer for the wrong one.
func newWith(w io.Writer, profile termenv.Profile) *UI {
	u := &UI{w: w, width: 80, plain: profile == termenv.Ascii}
	if f, ok := w.(*os.File); ok {
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 20 {
			u.width = cols
		}
	}
	// COLUMNS wins when set: it is how a caller states a width the fd cannot
	// report — a pty of a different size, or output forced to stay coloured while
	// being captured. Ignored unless it parses to something usable.
	if cols, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && cols > 20 {
		u.width = cols
	}
	r := lipgloss.NewRenderer(w)
	r.SetColorProfile(profile)

	// Adaptive colours, because a palette picked for a dark terminal is
	// unreadable on a light one and half of these are meant to recede.
	accent := lipgloss.AdaptiveColor{Light: "#6C3FBF", Dark: "#B388FF"}
	muted := lipgloss.AdaptiveColor{Light: "#6E6E6E", Dark: "#9A9A9A"}
	rule := lipgloss.AdaptiveColor{Light: "#D0D0D0", Dark: "#4A4A4A"}

	u.title = r.NewStyle().Bold(true).Foreground(accent)
	u.group = r.NewStyle().Bold(true).Foreground(accent)
	u.name = r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#005F87", Dark: "#7FD1FF"})
	u.desc = r.NewStyle().Foreground(muted)
	u.border = r.NewStyle().Foreground(rule)
	u.errWord = r.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#B00020", Dark: "#FF6B6B"})
	u.dim = r.NewStyle().Foreground(muted)
	return u
}

// Plain reports whether output is unstyled. Exported so the caller can say so in
// --help, and so tests can assert the decision rather than infer it from bytes.
func (u *UI) Plain() bool { return u.plain }

// SetPlain forces plain output — what --no-color binds to. A flag has to beat
// detection: a terminal that reports colour support can still be piped into
// something that stores the bytes.
func (u *UI) SetPlain(plain bool) { u.plain = plain }

// List writes the task listing.
//
// Plain output is byte-for-byte what chore has always printed:
//
//	tasks:
//
//	  [instance]
//	    instance:up      Bring up the full stack
//
// The name column is padded to the widest name ACROSS ALL GROUPS, so
// descriptions line up down the whole listing rather than per group.
func (u *UI) List(groups []Group) { u.ListUnder("tasks", groups) }

// ListUnder is List with the heading named, so the same layout can present
// something that is not a task list — `chore help` lists manual topics, and a
// listing headed "tasks:" would be telling the reader the wrong thing about
// every line under it.
func (u *UI) ListUnder(heading string, groups []Group) {
	if len(groups) == 0 {
		fmt.Fprintln(u.w, "no "+heading)
		return
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	for _, g := range groups {
		sort.Slice(g.Tasks, func(i, j int) bool { return g.Tasks[i].Name < g.Tasks[j].Name })
	}

	width := 0
	for _, g := range groups {
		for _, t := range g.Tasks {
			if w := lipgloss.Width(t.Name); w > width {
				width = w
			}
		}
	}

	if u.plain {
		u.listPlain(heading, groups, width)
		return
	}
	u.listStyled(heading, groups, width)
}

func (u *UI) listPlain(heading string, groups []Group, width int) {
	fmt.Fprintln(u.w, heading+":")
	for _, g := range groups {
		fmt.Fprintf(u.w, "\n  [%s]\n", g.Name)
		for _, t := range g.Tasks {
			if t.Desc == "" {
				fmt.Fprintf(u.w, "    %s\n", t.Name)
				continue
			}
			// Pad in display cells rather than with %-*s, which counts bytes.
			pad := width - lipgloss.Width(t.Name)
			fmt.Fprintf(u.w, "    %s%s  %s\n", t.Name, strings.Repeat(" ", pad), t.Desc)
		}
	}
}

// listStyled draws each namespace as its own block: a heading, a rule, then the
// tasks with descriptions in a receding colour.
//
// No box borders around 191 tasks. The point of the listing is to be scanned,
// and a table drawn per namespace turns a long project into thirty framed boxes
// — chrome competing with the only content that matters. A heading, one rule,
// and aligned columns do the same job with none of that.
func (u *UI) listStyled(heading string, groups []Group, width int) {
	// Truncate descriptions rather than let them wrap: a wrapped listing loses
	// the column that makes it scannable. 4 indent + name + 2 gap.
	descWidth := u.width - 4 - width - 2
	if descWidth < 12 {
		descWidth = 12
	}

	fmt.Fprintln(u.w, u.title.Render(heading))
	for _, g := range groups {
		rule := strings.Repeat("─", min(u.width-2, 38))
		fmt.Fprintf(u.w, "\n  %s\n  %s\n", u.group.Render(g.Name), u.border.Render(rule))
		for _, t := range g.Tasks {
			name := u.name.Render(t.Name)
			if t.Desc == "" {
				fmt.Fprintf(u.w, "    %s\n", name)
				continue
			}
			pad := strings.Repeat(" ", width-lipgloss.Width(t.Name))
			desc := t.Desc
			if lipgloss.Width(desc) > descWidth {
				desc = truncate(desc, descWidth)
			}
			fmt.Fprintf(u.w, "    %s%s  %s\n", name, pad, u.desc.Render(desc))
		}
	}
}

// Title writes a heading with an optional right-aligned note, e.g. a version.
func (u *UI) Title(left, right string) {
	if u.plain {
		if right == "" {
			fmt.Fprintln(u.w, left)
			return
		}
		fmt.Fprintf(u.w, "%s  %s\n", left, right)
		return
	}
	gap := u.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		gap = 2
	}
	fmt.Fprintf(u.w, "%s%s%s\n", u.title.Render(left), strings.Repeat(" ", gap), u.dim.Render(right))
}

// Errorf writes a message prefixed with the program name, the way every
// diagnostic in chore already looks. In plain mode the bytes are unchanged —
// "chore: " then the message — because callers' tests match on exactly that.
func (u *UI) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if u.plain {
		fmt.Fprintf(u.w, "chore: %s\n", msg)
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.errWord.Render("chore:"), msg)
}

// Dim writes a line that is present but should not compete — the note about
// reading a Taskfile.yml, or a --dry echo.
func (u *UI) Dim(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if u.plain {
		fmt.Fprintln(u.w, msg)
		return
	}
	fmt.Fprintln(u.w, u.dim.Render(msg))
}

// Raw writes without styling, for text that is already exactly what it must be
// (the usage block, a version string).
func (u *UI) Raw(s string) { fmt.Fprint(u.w, s) }

// truncate cuts a string to a cell width, leaving room for the ellipsis. Counted
// in runes over display width so a wide character is never split down the middle.
func truncate(s string, width int) string {
	if width <= 1 {
		return "…"
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if used+w > width-1 {
			b.WriteString("…")
			return b.String()
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}

// Banner announces which binary is running — printed to STDERR so it never
// contaminates a task's output or anything parsing stdout.
//
// Only for dev builds, and only on a terminal. A line above every command gets
// old fast, and the thing actually worth knowing is that you are NOT running the
// installed release: `chore build` doing something surprising is much easier to
// explain once you can see `dev+a581449-dirty` above it.
func (u *UI) Banner(name, version string) {
	if u.plain {
		return
	}
	fmt.Fprintf(u.w, "%s %s\n", u.title.Render(name), u.dim.Render(version))
}

// Detail writes an aligned label/value block — what `--version` shows beyond the
// bare version line. Styled to recede, since it is context rather than an answer.
func (u *UI) Detail(rows [][2]string) {
	width := 0
	for _, r := range rows {
		if w := lipgloss.Width(r[0]); w > width {
			width = w
		}
	}
	for _, r := range rows {
		if r[1] == "" {
			continue
		}
		label := r[0] + strings.Repeat(" ", width-lipgloss.Width(r[0]))
		if u.plain {
			fmt.Fprintf(u.w, "  %s  %s\n", label, r[1])
			continue
		}
		// Label dim, value in the terminal's own foreground: styling BOTH the same
		// muted colour is what the first version did, and it made the labels and
		// the answers indistinguishable.
		fmt.Fprintf(u.w, "  %s  %s\n", u.dim.Render(label), r[1])
	}
}
