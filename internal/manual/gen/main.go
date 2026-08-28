// Command gen extracts the built-in manual from `chore:manual` comment blocks in
// the source and writes one markdown file per topic into internal/manual/topics.
//
// Run it with `chore manual` (or `go generate ./...`). CI regenerates and fails
// on a diff, which is the only thing that actually keeps the manual in sync with
// the code: a document that lives beside the code it describes still rots if
// nothing checks it.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antimatter-studios/chore/internal/manual"
)

func main() {
	root, out := ".", filepath.Join("internal", "manual", "topics")
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	if len(os.Args) > 2 {
		out = os.Args[2]
	}
	if err := run(root, out); err != nil {
		fmt.Fprintf(os.Stderr, "manual: %v\n", err)
		os.Exit(1)
	}
}

func run(root, out string) error {
	blocks, err := collect(root)
	if err != nil {
		return err
	}
	topics, err := assemble(blocks)
	if err != nil {
		return err
	}
	return write(out, topics)
}

// block is one `chore:manual` comment group: which topic it belongs to, where it
// came from, and the markdown it carries.
type block struct {
	topic   string
	title   string
	summary string
	aliases []string
	order   int
	file    string
	line    int
	body    []string
}

// collect walks the tree for .go files and pulls every manual block out of them.
//
// Test files are skipped: a manual is a statement about the shipped program, and
// a block in a _test.go file would document behaviour no user can reach.
func collect(root string) ([]block, error) {
	var out []block
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under a dot-directory or vendor/ is this program's source.
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "bin" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, cg := range f.Comments {
			b, ok, err := parseBlock(cg, filepath.ToSlash(path), fset.Position(cg.Pos()).Line)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, b)
			}
		}
		return nil
	})
	return out, err
}

// parseBlock reads one comment group, returning false if it is not a manual
// block.
//
// The lines are un-commented by hand rather than with CommentGroup.Text, which
// drops "directive" lines and collapses the blank lines and indentation that
// markdown depends on. Exactly one leading space is removed, so a fenced code
// block written at the margin stays at the margin and an indented one keeps its
// indent.
func parseBlock(cg *ast.CommentGroup, file string, line int) (block, bool, error) {
	var lines []string
	for _, c := range cg.List {
		switch {
		case strings.HasPrefix(c.Text, "//"):
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), " "))
		case strings.HasPrefix(c.Text, "/*"):
			inner := strings.TrimSuffix(strings.TrimPrefix(c.Text, "/*"), "*/")
			for _, l := range strings.Split(inner, "\n") {
				lines = append(lines, strings.TrimPrefix(l, " "))
			}
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	if len(lines) == 0 {
		return block{}, false, nil
	}

	head := strings.Fields(strings.TrimSpace(lines[0]))
	if len(head) == 0 || head[0] != marker {
		// A marker anywhere BUT the first line is refused rather than ignored. It
		// means the block was written against a declaration that already had a doc
		// comment, so the two merged into one group — and the topic then vanishes
		// from the manual without a word, which is the failure this program exists
		// to eliminate. It has happened once already, to `includes`.
		for i, l := range lines[1:] {
			if f := strings.Fields(strings.TrimSpace(l)); len(f) > 0 && f[0] == marker {
				return block{}, false, fmt.Errorf("%s:%d: `%s` is on line %d of a comment block instead of the first —"+
					" separate it from the comment above with a blank line, or it merges into that comment and is silently dropped",
					file, line, marker, i+2)
			}
		}
		return block{}, false, nil
	}
	if len(head) != 2 {
		return block{}, false, fmt.Errorf("%s:%d: `%s` needs exactly one topic name, got %q",
			file, line, marker, strings.TrimSpace(lines[0]))
	}
	b := block{topic: head[1], file: file, line: line}
	if err := manual.ValidTopic(b.topic); err != nil {
		return block{}, false, fmt.Errorf("%s:%d: %w", file, line, err)
	}

	// Optional `key: value` headers, ending at the first blank line or the first
	// line that is not one. Only three keys exist, and an unknown one is an error
	// rather than prose, because a mistyped `sumary:` would silently become the
	// topic's first paragraph.
	rest := lines[1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		key, value, ok := strings.Cut(rest[0], ":")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		if strings.ContainsAny(key, " \t") {
			break
		}
		value = strings.TrimSpace(value)
		switch key {
		case "title":
			b.title = value
		case "summary":
			b.summary = value
		case "aliases":
			for _, a := range strings.Fields(strings.ReplaceAll(value, ",", " ")) {
				if err := manual.ValidTopic(a); err != nil {
					return block{}, false, fmt.Errorf("%s:%d: alias: %w", file, line, err)
				}
				b.aliases = append(b.aliases, a)
			}
		case "order":
			n, err := parseOrder(value)
			if err != nil {
				return block{}, false, fmt.Errorf("%s:%d: %w", file, line, err)
			}
			b.order = n
		default:
			return block{}, false, fmt.Errorf("%s:%d: unknown header %q in a %s block — expected title, summary, aliases or order",
				file, line, key, marker)
		}
		rest = rest[1:]
	}
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	for len(rest) > 0 && strings.TrimSpace(rest[len(rest)-1]) == "" {
		rest = rest[:len(rest)-1]
	}
	if len(rest) == 0 {
		return block{}, false, fmt.Errorf("%s:%d: %s %s has no text", file, line, marker, b.topic)
	}
	b.body = rest
	return b, true, nil
}

const marker = "chore:manual"

func parseOrder(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("`order:` needs a number")
	}
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("`order: %s` is not a number", s)
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// assemble groups blocks into topics and puts their sections in a defined order:
// `order:` first, then file path, then line. Two sections that do not say which
// comes first must still come out the same way on every machine, or the
// regenerate-and-diff check in CI fires on nothing.
func assemble(blocks []block) ([]manual.Topic, error) {
	byTopic := map[string][]block{}
	for _, b := range blocks {
		byTopic[b.topic] = append(byTopic[b.topic], b)
	}

	var out []manual.Topic
	for name, bs := range byTopic {
		sort.SliceStable(bs, func(i, j int) bool {
			if bs[i].order != bs[j].order {
				return bs[i].order < bs[j].order
			}
			if bs[i].file != bs[j].file {
				return bs[i].file < bs[j].file
			}
			return bs[i].line < bs[j].line
		})

		t := manual.Topic{Name: name}
		var body []string
		for _, b := range bs {
			// A topic's title and summary are written once. Two blocks disagreeing
			// is a mistake worth stopping for: silently keeping the first would make
			// the listing depend on file order.
			if err := single(&t.Title, b.title, name, "title", b); err != nil {
				return nil, err
			}
			if err := single(&t.Summary, b.summary, name, "summary", b); err != nil {
				return nil, err
			}
			t.Aliases = append(t.Aliases, b.aliases...)
			if len(body) > 0 {
				body = append(body, "")
			}
			body = append(body, b.body...)
			t.Sources = append(t.Sources, fmt.Sprintf("%s:%d", b.file, b.line))
		}
		if t.Title == "" {
			return nil, fmt.Errorf("topic %q has no `title:` — one of its blocks must carry it", name)
		}
		if t.Summary == "" {
			return nil, fmt.Errorf("topic %q has no `summary:` — one of its blocks must carry it, it is what `chore help` lists", name)
		}
		t.Body = strings.Join(body, "\n") + "\n"
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// An alias that collides is worse than no alias: it makes `chore help X`
	// answer with something the reader did not ask for, and which of the two it
	// picks would depend on iteration order.
	seen := map[string]string{}
	for _, t := range out {
		seen[t.Name] = t.Name
	}
	for _, t := range out {
		for _, a := range t.Aliases {
			if owner, dup := seen[a]; dup {
				return nil, fmt.Errorf("alias %q on topic %q is already taken by %q", a, t.Name, owner)
			}
			seen[a] = t.Name
		}
	}
	return out, nil
}

func single(dst *string, value, topic, what string, b block) error {
	if value == "" {
		return nil
	}
	if *dst != "" && *dst != value {
		return fmt.Errorf("topic %q has two different %ss (%q and %q); %s:%d",
			topic, what, *dst, value, b.file, b.line)
	}
	*dst = value
	return nil
}

// write replaces the topics directory wholesale. A topic deleted from the source
// has to disappear from the binary, and only rewriting the files that still
// exist would leave it behind for ever — embedded, listed, and describing
// something that no longer happens.
func write(dir string, topics []manual.Topic) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	existing, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, t := range topics {
		keep[filepath.Join(dir, t.Name+".md")] = true
	}
	for _, path := range existing {
		if !keep[path] {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	for _, t := range topics {
		var b strings.Builder
		b.WriteString("<!-- Generated from `" + marker + "` comments. Do not edit; run `chore manual`. -->\n")
		b.WriteString("<!-- sources: " + strings.Join(t.Sources, " ") + " -->\n")
		b.WriteString("---\n")
		b.WriteString("title: " + t.Title + "\n")
		b.WriteString("summary: " + t.Summary + "\n")
		if len(t.Aliases) > 0 {
			b.WriteString("aliases: " + strings.Join(t.Aliases, " ") + "\n")
		}
		b.WriteString("---\n\n")
		b.WriteString(t.Body)
		path := filepath.Join(dir, t.Name+".md")
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}
