// Package manual is the built-in manual: the topic pages compiled into the
// binary, and the lookup `chore help` uses.
//
// The pages are GENERATED from `chore:manual` comment blocks in the source —
// see internal/manual/gen. Nothing here parses Go; this package only reads what
// the generator wrote, so the binary carries the manual with no build tags and
// no files to install.
//
// Why the blocks live beside the code rather than in a docs/ directory: a
// document in another file is a copy, and a copy of a behaviour drifts from it
// silently. Beside the code, the paragraph describing a rule is in the diff that
// changes the rule.
package manual

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed topics/*.md
var files embed.FS

// chore:manual manual
// title: The built-in manual
// summary: how `chore help` works, and how to add a topic
// order: 10
//
// # The built-in manual
//
// ```
// chore help              list the topics
// chore help hooks        print one
// ```
//
// Underscores and hyphens are interchangeable, because the name is a phrase
// half-remembered rather than an identifier copied: `chore help lifecycle_hooks`
// and `chore help lifecycle-hooks` are the same question.
//
// ## How it is built
//
// The pages are EXTRACTED from the source. A topic section is a standalone
// comment block sitting next to the code that implements it:
//
// ```go
// // chore:manual hooks
// // title: Hooks
// // summary: before/on_success/on_failure/after, on a task or the whole run
// // order: 10
// //
// // # Hooks
// //
// // ...markdown...
// ```
//
// `chore manual` regenerates `internal/manual/topics/*.md`, which are embedded
// into the binary. CI regenerates and fails on a diff.
//
// - `title:` and `summary:` are written ONCE per topic, on any one of its blocks.
//   Two blocks disagreeing is an error rather than a silent first-wins, which
//   would make the listing depend on file order.
// - `order:` sorts sections within a topic; ties break on file path, then line. A
//   topic can therefore be assembled from several files and still come out the
//   same way on every machine, which is what makes the CI diff meaningful.
// - The block must be SEPARATED from any declaration by a blank line. Attached to
//   one it becomes a Go doc comment, and gofmt reformats doc comments —
//   re-indenting code blocks and rewriting list markers, which mangles markdown.
//
// The point is that the paragraph describing a rule lives in the same diff as the
// rule. A document in another file is a copy, and a copy drifts silently.

// Topic is one manual page.
type Topic struct {
	// Name is the page's address — what a reader types after `chore help`.
	Name string
	// Title and Summary come from the block's headers. Summary is the one-line
	// description `chore help` lists, so a topic without one would be invisible in
	// the only place a reader goes looking; the generator refuses that.
	Title   string
	Summary string
	// Aliases are other names that reach this page. They exist because a topic's
	// best name and the phrase someone reaches for are not always the same word:
	// the page is `hooks`, but a reader who just read a CHANGELOG entry types
	// `lifecycle_hooks`, and answering "no such topic" to a question the manual
	// plainly covers teaches nothing.
	Aliases []string
	// Body is the markdown, with the headers and the generated preamble stripped.
	Body string
	// Sources are the `file:line` positions of the blocks this page was built
	// from, in the order they were concatenated. Recorded by the generator so a
	// reader who wants the rule's implementation can find it, and so a page can
	// be traced back when it looks wrong.
	Sources []string
}

// All returns every topic, ordered by name.
func All() []Topic {
	entries, err := fs.ReadDir(files, "topics")
	if err != nil {
		return nil
	}
	var out []Topic
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		t, err := load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup finds one topic by name.
//
// Hyphens and underscores are interchangeable, because the name is a phrase a
// reader half-remembers rather than an identifier they copied: `chore help
// lifecycle_hooks` and `chore help lifecycle-hooks` are the same question, and
// refusing one of them teaches nothing.
func Lookup(name string) (Topic, bool) {
	want := normalise(name)
	for _, t := range All() {
		if normalise(t.Name) == want {
			return t, true
		}
		for _, a := range t.Aliases {
			if normalise(a) == want {
				return t, true
			}
		}
	}
	return Topic{}, false
}

// Suggest returns the topics closest to a name that did not match, so a near
// miss answers the question instead of listing everything.
func Suggest(name string) []string {
	want := normalise(name)
	var out []string
	for _, t := range All() {
		n := normalise(t.Name)
		if strings.Contains(n, want) || strings.Contains(want, n) {
			out = append(out, t.Name)
		}
	}
	return out
}

func normalise(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "_", "-")
}

// ValidTopic reports whether a name can be a topic. Kept here rather than in the
// generator because it defines what a reader is allowed to type.
func ValidTopic(name string) error {
	if name == "" {
		return fmt.Errorf("a topic name cannot be empty")
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0 && i < len(name)-1:
		default:
			return fmt.Errorf("topic %q is not usable: a topic name is lowercase letters, digits,"+
				" and inner hyphens or underscores — it is what someone types after `chore help`", name)
		}
	}
	return nil
}

// load reads one generated page. The front matter is the generator's own format,
// so a parse failure here means the generated file was hand-edited.
func load(file string) (Topic, error) {
	data, err := files.ReadFile(path.Join("topics", file))
	if err != nil {
		return Topic{}, err
	}
	t := Topic{Name: strings.TrimSuffix(file, ".md")}
	lines := strings.Split(string(data), "\n")

	for len(lines) > 0 && strings.HasPrefix(lines[0], "<!--") {
		if rest, ok := strings.CutPrefix(lines[0], "<!-- sources:"); ok {
			t.Sources = strings.Fields(strings.TrimSuffix(strings.TrimSpace(rest), "-->"))
		}
		lines = lines[1:]
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Topic{}, fmt.Errorf("%s: no front matter", file)
	}
	lines = lines[1:]
	for len(lines) > 0 && strings.TrimSpace(lines[0]) != "---" {
		key, value, _ := strings.Cut(lines[0], ":")
		switch strings.TrimSpace(key) {
		case "title":
			t.Title = strings.TrimSpace(value)
		case "summary":
			t.Summary = strings.TrimSpace(value)
		case "aliases":
			t.Aliases = strings.Fields(value)
		}
		lines = lines[1:]
	}
	if len(lines) > 0 {
		lines = lines[1:] // the closing ---
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	t.Body = strings.TrimRight(strings.Join(lines, "\n"), "\n")
	return t, nil
}
