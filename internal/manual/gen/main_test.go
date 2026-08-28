package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/manual"
)

// parse runs one file's worth of source through the block reader, which is where
// every rule about the comment format lives.
func parse(t *testing.T, src string) ([]block, error) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", "package x\n\n"+src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the fixture itself failed: %v", err)
	}
	var out []block
	for _, cg := range f.Comments {
		b, ok, err := parseBlock(cg, "x.go", fset.Position(cg.Pos()).Line)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, b)
		}
	}
	return out, nil
}

func TestParseBlockReadsHeadersAndBody(t *testing.T) {
	bs, err := parse(t, `
// chore:manual hooks
// title: Hooks
// summary: what runs around a task
// aliases: lifecycle-hooks, lifecycle
// order: 20
//
// # Hooks
//
// `+"```yaml"+`
// build:
//   before: [ ./check.sh ]
// `+"```"+`
`)
	if err != nil {
		t.Fatalf("parseBlock: %v", err)
	}
	if len(bs) != 1 {
		t.Fatalf("got %d blocks, want 1", len(bs))
	}
	b := bs[0]
	if b.topic != "hooks" || b.title != "Hooks" || b.order != 20 {
		t.Errorf("headers = %+v", b)
	}
	if strings.Join(b.aliases, ",") != "lifecycle-hooks,lifecycle" {
		t.Errorf("aliases = %v", b.aliases)
	}
	// Indentation inside a fenced block is what makes the YAML sample legible, so
	// exactly one leading space is removed and no more.
	want := "# Hooks\n\n```yaml\nbuild:\n  before: [ ./check.sh ]\n```"
	if got := strings.Join(b.body, "\n"); got != want {
		t.Errorf("body =\n%s\n\nwant\n%s", got, want)
	}
}

func TestParseBlockIgnoresOrdinaryComments(t *testing.T) {
	bs, err := parse(t, "// Just a normal comment about the code.\n// It mentions no marker at the start of any line.\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 0 {
		t.Fatalf("an ordinary comment must not become a manual block: %+v", bs)
	}
}

// The failure this closes actually happened: a block written directly under an
// existing doc comment merged with it, so its first line was no longer the
// marker, and the `includes` topic disappeared from the manual with no error
// anywhere. Silently dropping it is the one outcome that must not be possible.
func TestParseBlockRefusesAMarkerThatIsNotFirst(t *testing.T) {
	_, err := parse(t, "// Load reads the taskfile.\n// chore:manual includes\n// title: Includes\n//\n// body\n")
	if err == nil {
		t.Fatal("a marker below the first line must be an error, not a silent skip")
	}
	if !strings.Contains(err.Error(), "separate it from the comment above with a blank line") {
		t.Fatalf("error = %q, want it to name the fix", err)
	}
}

func TestParseBlockRejectsMalformedHeaders(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{
			name: "two topic names",
			src:  "// chore:manual a b\n// title: T\n//\n// body\n",
			want: "exactly one topic name",
		},
		{
			// A mistyped header would otherwise become the topic's first paragraph,
			// and the missing summary would only show up as a blank listing column.
			name: "unknown header",
			src:  "// chore:manual a\n// sumary: oops\n//\n// body\n",
			want: `unknown header "sumary"`,
		},
		{
			name: "non-numeric order",
			src:  "// chore:manual a\n// order: soon\n//\n// body\n",
			want: "is not a number",
		},
		{
			name: "unusable topic name",
			src:  "// chore:manual Not A Topic\n//\n// body\n",
			want: "exactly one topic name",
		},
		{
			name: "uppercase topic",
			src:  "// chore:manual Hooks\n//\n// body\n",
			want: "is not usable",
		},
		{
			name: "no text",
			src:  "// chore:manual a\n// title: T\n",
			want: "has no text",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.src)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestAssembleOrdersSectionsDeterministically(t *testing.T) {
	// Deliberately out of order, and with a tie on `order:` broken by file then
	// line — the property the CI diff depends on.
	blocks := []block{
		{topic: "t", order: 30, file: "b.go", line: 1, body: []string{"third"}},
		{topic: "t", title: "T", summary: "s", order: 10, file: "z.go", line: 9, body: []string{"first"}},
		{topic: "t", order: 20, file: "a.go", line: 5, body: []string{"second-a"}},
		{topic: "t", order: 20, file: "a.go", line: 1, body: []string{"second-b"}},
	}
	got, err := assemble(blocks)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d topics, want 1", len(got))
	}
	want := "first\n\nsecond-b\n\nsecond-a\n\nthird\n"
	if got[0].Body != want {
		t.Errorf("body = %q, want %q", got[0].Body, want)
	}
	wantSrc := "z.go:9 a.go:1 a.go:5 b.go:1"
	if strings.Join(got[0].Sources, " ") != wantSrc {
		t.Errorf("sources = %v, want %s", got[0].Sources, wantSrc)
	}
}

func TestAssembleRefusesAmbiguousOrIncompleteTopics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []block
		want   string
	}{
		{
			name: "two titles",
			blocks: []block{
				{topic: "t", title: "One", summary: "s", file: "a.go", body: []string{"x"}},
				{topic: "t", title: "Two", file: "b.go", body: []string{"y"}},
			},
			want: "two different titles",
		},
		{
			name:   "no title",
			blocks: []block{{topic: "t", summary: "s", file: "a.go", body: []string{"x"}}},
			want:   "has no `title:`",
		},
		{
			// Without one the topic is a blank line in `chore help`, which is the
			// only place anyone goes looking for it.
			name:   "no summary",
			blocks: []block{{topic: "t", title: "T", file: "a.go", body: []string{"x"}}},
			want:   "has no `summary:`",
		},
		{
			name: "alias collides with a topic",
			blocks: []block{
				{topic: "hooks", title: "H", summary: "s", file: "a.go", body: []string{"x"}},
				{topic: "defer", title: "D", summary: "s", aliases: []string{"hooks"}, file: "b.go", body: []string{"y"}},
			},
			want: "already taken",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := assemble(tc.blocks)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A topic removed from the source has to disappear from the binary. Rewriting
// only the files that still exist would leave it embedded, listed, and
// describing something that no longer happens.
func TestWriteRemovesTopicsThatNoLongerExist(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "gone.md")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := []manual.Topic{{Name: "kept", Title: "Kept", Summary: "still here", Body: "body\n"}}
	if err := write(dir, kept); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a topic no longer in the source must be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "kept.md")); err != nil {
		t.Errorf("the surviving topic should have been written: %v", err)
	}
}

// End to end over the real repository: the generator must run clean on the tree
// it ships with, which is also what proves every committed block parses.
func TestCollectOverTheRepository(t *testing.T) {
	blocks, err := collect("../../..")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(blocks) == 0 {
		t.Fatal("no manual blocks found in the repository")
	}
	topics, err := assemble(blocks)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	seen := map[string]bool{}
	for _, top := range topics {
		seen[top.Name] = true
	}
	// The hooks page is assembled from two packages, which is the whole reason
	// `order:` exists; if that stops working the page silently loses half itself.
	if !seen["hooks"] {
		t.Errorf("expected a hooks topic, got %v", seen)
	}
}
