// The gate is demonstrated by its failures, not by its passes.
//
// A guard that has only ever been seen to pass is indistinguishable from a
// guard that reads nothing: both are green. Every check here is therefore proved
// by MUTATION — take a real repository's real `ci.yml` and `.github-guard`, make
// exactly the edit the check exists to catch, and assert that check fires and
// names the cost.
//
// The configurations under testdata/ are verbatim copies, not sketches. A
// hand-written fixture only proves the guard handles the shape its author had in
// mind, which is the shape the guard was written against — the two agree because
// they came from one head, not because the rule holds. They came from:
//
//	fixture          repository                        commit
//	rust-fs-xfs      antimatter-studios/rust-fs-xfs    8dbbc57
//	rust-partitions  antimatter-studios/rust-partitions 01309d2
//	rust-fs-ntfs     antimatter-studios/rust-fs-ntfs   2889a64
//
// xfs and partitions are the two the mutations run against: they differ in job
// count (six and four), in matrix shape, in whether a darwin runner is present,
// and partitions carries a third workflow (`fuzz.yml`) that xfs does not. ntfs
// is here because it is the only repository in the family with genuinely
// non-gating jobs, and so the only real configuration that exercises the
// per-repo exemption.
package cigate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antimatter-studios/chore/internal/chorefile"
)

const (
	xfs        = "rust-fs-xfs"
	partitions = "rust-partitions"
	ntfs       = "rust-fs-ntfs"
)

// mutated are the two repositories the mutations run against.
var mutated = []string{xfs, partitions}

// needsOf is each fixture's real `needs:` line, so a mutation names the line it
// is editing rather than a substring that might match twice.
var needsOf = map[string]string{
	xfs:        "needs: [unit, fixtures, test, test-arm64, test-darwin, suite-in-vm]",
	partitions: "needs: [test, test-release, oracle, fmt]",
}

// ntfsRequired is ntfs's real required list: the five named check names it
// declares instead of an aggregate. Replaced wholesale where a test gives it the
// aggregate it does not have yet — swapping only the first line would leave four
// more required checks behind, and the test would assert something other than
// what it says.
const ntfsRequired = "\trequired = test-ubuntu-latest\n" +
	"\trequired = test-ubuntu-24.04-arm\n" +
	"\trequired = test-macos-latest\n" +
	"\trequired = full test suite (fixtures + integration)\n" +
	"\trequired = validate rust-ntfs format (Windows chkdsk)"

// scratch is a throwaway copy of a fixture repository, mutable and removed with
// the test.
type scratch struct {
	t   *testing.T
	dir string
}

func of(t *testing.T, fixture string) *scratch {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".github-guard", ".github/workflows/ci.yml"} {
		b, err := os.ReadFile(filepath.Join("testdata", fixture, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &scratch{t: t, dir: dir}
}

func (s *scratch) path(rel string) string { return filepath.Join(s.dir, filepath.FromSlash(rel)) }

// mutate replaces the first occurrence of from with to, refusing if it is not
// there. A mutation that did not apply proves nothing, and a silent no-op would
// leave this file asserting that an UNCHANGED config fails — the exact inversion
// it exists to rule out.
func (s *scratch) mutate(rel, from, to string) *scratch {
	s.t.Helper()
	b, err := os.ReadFile(s.path(rel))
	if err != nil {
		s.t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, from) {
		s.t.Fatalf("the mutation does not apply: %s in this fixture has no %q. "+
			"The fixture has moved on and this test is no longer mutating anything.", rel, from)
	}
	if err := os.WriteFile(s.path(rel), []byte(strings.Replace(text, from, to, 1)), 0o644); err != nil {
		s.t.Fatal(err)
	}
	return s
}

func (s *scratch) check(cfg chorefile.CIGate) []Failure { return Check(s.dir, cfg) }

// fires asserts that exactly the named check fired, and that its message says
// what it cost. Checking the message is not decoration: a check that fires with
// an unhelpful message is a check whose failure gets silenced rather than fixed.
func fires(t *testing.T, failures []Failure, check, needle string) {
	t.Helper()
	var hit []Failure
	for _, f := range failures {
		if f.Check == check {
			hit = append(hit, f)
		}
	}
	if len(hit) == 0 {
		t.Fatalf("expected %q to fire; what fired instead: %v", check, failures)
	}
	for _, f := range hit {
		if strings.Contains(f.Detail, needle) {
			return
		}
	}
	t.Fatalf("%q fired but did not explain %q: %v", check, needle, hit)
}

func passes(t *testing.T, failures []Failure, why string) {
	t.Helper()
	if len(failures) != 0 {
		t.Fatalf("%s: %v", why, failures)
	}
}

// ---------------------------------------------------------------------
// The baseline. Everything below is only meaningful because of this.
// ---------------------------------------------------------------------

// Without this the mutation tests would also pass against a guard that fails on
// absolutely everything.
func TestTheRealConfigurationsPassUnmodified(t *testing.T) {
	for _, fixture := range mutated {
		s := of(t, fixture)
		passes(t, s.check(chorefile.CIGate{}), fixture+" gates correctly today and the guard should say so")
	}
}

// ---------------------------------------------------------------------
// Mutation 1 — a job drops out of `needs:`.
// ---------------------------------------------------------------------

// The job is still in `ci.yml`, still runs, still reports. It just no longer
// gates, and nothing else in either repository would notice.
func TestRemovingAJobFromNeedsFires(t *testing.T) {
	// xfs: six jobs behind the gate. Drop the macOS leg — the one whose absence
	// is least visible, because the other five still run.
	x := of(t, xfs).mutate(".github/workflows/ci.yml", needsOf[xfs],
		"needs: [unit, fixtures, test, test-arm64, suite-in-vm]")
	fires(t, x.check(chorefile.CIGate{}), checkNeedsEveryJob,
		"`ci-ok` does not need `test-darwin`, so that job gates nothing")

	// partitions: four jobs, and a different one — the oracle suite that checks
	// its tables against sgdisk, sfdisk, blkid and partx.
	p := of(t, partitions).mutate(".github/workflows/ci.yml", needsOf[partitions],
		"needs: [test, test-release, fmt]")
	fires(t, p.check(chorefile.CIGate{}), checkNeedsEveryJob,
		"`ci-ok` does not need `oracle`, so that job gates nothing")
}

// `needs:` emptied entirely: the aggregate reports success having asked nobody.
// This is the failure that looks most like success.
func TestAnAggregateThatNeedsNothingFires(t *testing.T) {
	x := of(t, xfs).mutate(".github/workflows/ci.yml", needsOf[xfs], "needs: []")
	fires(t, x.check(chorefile.CIGate{}), checkNeedsEveryJob,
		"needs nothing, so it says every job succeeded while asking none of them")
}

// A `needs:` entry naming a job that is not there. GitHub refuses to run the
// workflow at all, so the one required check never reports — and a required
// check that never reports is a permanent block on every merge.
func TestNeedingAJobThatDoesNotExistFires(t *testing.T) {
	p := of(t, partitions).mutate(".github/workflows/ci.yml", needsOf[partitions],
		"needs: [test, test-release, oracle, fmt, clippy]")
	fires(t, p.check(chorefile.CIGate{}), checkNeedsEveryJob,
		"`ci-ok` needs `clippy`, which is not a job in")
}

// ---------------------------------------------------------------------
// Mutation 2 — `if: always()` comes off the aggregate.
// ---------------------------------------------------------------------

// Without it the aggregate is skipped whenever anything it needs was cancelled
// or skipped, and a skipped required check never reports.
func TestDeletingIfAlwaysFires(t *testing.T) {
	for _, fixture := range mutated {
		// The `if:` is written on the line before `needs:`, so mutate the pair —
		// xfs has five step-level `if: always()` above it.
		s := of(t, fixture).mutate(".github/workflows/ci.yml",
			"    if: always()\n    "+needsOf[fixture], "    "+needsOf[fixture])
		fires(t, s.check(chorefile.CIGate{}), checkRunsWhatever,
			"a skipped required check never reports")
	}
}

// `always()` narrowed to something that can be false. This is the subtler edit —
// the key is still there, so a reader skimming the diff sees `if:` where they
// expected `if:` — and the original line-scanning copies in all eleven
// repositories accepted it, because they looked for the exact text
// `if: always()` and reported only its absence.
func TestNarrowingAlwaysToAConditionThatCanBeFalseFires(t *testing.T) {
	for _, fixture := range mutated {
		s := of(t, fixture).mutate(".github/workflows/ci.yml",
			"    if: always()\n    "+needsOf[fixture],
			"    if: always() && github.event_name == 'pull_request'\n    "+needsOf[fixture])
		fires(t, s.check(chorefile.CIGate{}), checkRunsWhatever,
			"Any condition that can be false is a condition under which the one required check does not report")
	}
}

// `${{ always() }}` is the same expression with braces, and must pass. A guard
// that rejects a correct spelling gets worked around.
func TestTheBracedSpellingOfAlwaysPasses(t *testing.T) {
	for _, fixture := range mutated {
		s := of(t, fixture).mutate(".github/workflows/ci.yml",
			"    if: always()\n    "+needsOf[fixture],
			"    if: ${{ always() }}\n    "+needsOf[fixture])
		passes(t, s.check(chorefile.CIGate{}), fixture+": `${{ always() }}` is `always()`")
	}
}

// ---------------------------------------------------------------------
// Mutation 3 — `.github-guard` requires something else as well.
// ---------------------------------------------------------------------

// A second required check re-introduces the whole problem: that name is now
// pinned in a file the pull request renaming it cannot also change, because
// github-guard reads `.github-guard` from the DEFAULT BRANCH.
func TestASecondRequiredCheckFires(t *testing.T) {
	x := of(t, xfs).mutate(".github-guard", "\trequired = ci-ok",
		"\trequired = ci-ok\n\trequired = test / ubuntu-latest")
	fires(t, x.check(chorefile.CIGate{}), checkRequiresOne,
		`requires ["ci-ok" "test / ubuntu-latest"]`)

	p := of(t, partitions).mutate(".github-guard", "\trequired = ci-ok",
		"\trequired = ci-ok\n\trequired = fmt")
	fires(t, p.check(chorefile.CIGate{}), checkRequiresOne, `requires ["ci-ok" "fmt"]`)
}

// The aggregate dropped from the required list entirely: every job still runs,
// and none of them is required.
func TestRequiringSomethingOtherThanTheAggregateFires(t *testing.T) {
	p := of(t, partitions).mutate(".github-guard", "\trequired = ci-ok", "\trequired = fmt")
	fires(t, p.check(chorefile.CIGate{}), checkRequiresOne,
		"requires [\"fmt\"]; it should name `ci-ok` alone")
}

// The prose is not the declaration. Every one of these files opens with a long
// argued rationale, and partitions' contains the literal text `required` beside
// old check names. A scanner that takes any line containing `required =` reads
// an argument about what used to be required as a list of things that are.
func TestARequiredLineInsideACommentIsNotARequirement(t *testing.T) {
	p := of(t, partitions).mutate(".github-guard", "[checks]",
		"# a note about how this used to say `required = test / ubuntu-latest`\n[checks]")
	passes(t, p.check(chorefile.CIGate{}), "a comment is not a declaration")
}

// ---------------------------------------------------------------------
// Mutation 4 — the gate workflow stops gating pull requests.
// ---------------------------------------------------------------------

// Only a `pull_request` workflow can gate a pull request. None of the eleven
// copied guards checked this, and it is the premise every one of them rests on.
func TestAGateWorkflowThatDoesNotRunOnPullRequestsFires(t *testing.T) {
	x := of(t, xfs).mutate(".github/workflows/ci.yml", "  pull_request:", "  workflow_dispatch:")
	fires(t, x.check(chorefile.CIGate{}), checkRunsOnPR, "which GitHub reads as permanently pending")
}

// partitions' real `fuzz.yml` — `workflow_dispatch` plus a nightly cron —
// pointed at as the gate. It reports on no pull request ever, so requiring
// anything from it blocks every merge forever. This is the mistake the check
// exists to make impossible, on a real file.
func TestPointingTheGateAtTheNightlyFuzzWorkflowFires(t *testing.T) {
	s := of(t, partitions)
	b, err := os.ReadFile(filepath.Join("testdata", partitions, ".github", "workflows", "fuzz.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(".github/workflows/fuzz.yml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	fires(t, s.check(chorefile.CIGate{Workflow: ".github/workflows/fuzz.yml"}),
		checkRunsOnPR, "does not run on `pull_request`")
}

// ---------------------------------------------------------------------
// The per-repo exemption, on the only real configuration that has one.
// ---------------------------------------------------------------------

// rust-fs-ntfs has no aggregate job at all — its `.github-guard` names five real
// check names and argues, at its #278, that the aggregate is the better end
// state it has not adopted yet. The gate has to say so plainly rather than pass
// by finding nothing to complain about.
func TestARepositoryWithNoAggregateJobFires(t *testing.T) {
	n := of(t, ntfs)
	fires(t, n.check(chorefile.CIGate{}), checkNeedsEveryJob,
		"has no `ci-ok` job, so protection has to name every job by hand")
}

// ntfs's real jobs, given the aggregate it does not have yet.
//
// `changes` carries `if: github.event_name == 'pull_request'` and
// `validate-mkfs-windows` an `if:` restricting it to tag pushes and
// `workflow_dispatch`. Both are deliberately conditional. Needing them anyway
// means every pull request fails on a job that was never meant to run on one —
// the aggregate counts `skipped` as not-success, which is precisely what makes
// it worth having.
func TestNeedingAConditionalJobFires(t *testing.T) {
	n := of(t, ntfs).
		mutate(".github/workflows/ci.yml", "jobs:\n",
			"jobs:\n  ci-ok:\n    if: always()\n    needs: [test, integration, asan, changes, "+
				"validate-mkfs-windows]\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n").
		mutate(".github-guard", ntfsRequired, "\trequired = ci-ok")
	failures := n.check(chorefile.CIGate{})
	for _, job := range []string{"changes", "validate-mkfs-windows"} {
		fires(t, failures, checkNeedsEveryJob,
			"`"+job+"` carries [\"if\"] but is not declared non-gating")
	}
}

// Declaring them non-gating and taking them out of `needs:` is the fix, and the
// gate has to accept it — otherwise the only way past is to delete the guard.
func TestDeclaringTheConditionalJobsNonGatingPasses(t *testing.T) {
	n := of(t, ntfs).
		mutate(".github/workflows/ci.yml", "jobs:\n",
			"jobs:\n  ci-ok:\n    if: always()\n    needs: [test, integration, asan]\n    "+
				"runs-on: ubuntu-latest\n    steps:\n      - run: true\n").
		mutate(".github-guard", ntfsRequired, "\trequired = ci-ok")
	passes(t, n.check(chorefile.CIGate{NonGating: []string{"changes", "validate-mkfs-windows"}}),
		"a declared, genuinely conditional job is the supported shape")
}

// The exemption list cannot drift away from the workflow. Declaring a job
// non-gating when it carries no conditional key takes a working gate off a job
// whose failures are real — which is how an exemption written for one situation
// outlives it.
func TestExemptingAJobThatIsNotConditionalFires(t *testing.T) {
	p := of(t, partitions).mutate(".github/workflows/ci.yml", needsOf[partitions],
		"needs: [test, test-release, fmt]")
	fires(t, p.check(chorefile.CIGate{NonGating: []string{"oracle"}}), checkNeedsEveryJob,
		"`oracle` is declared non-gating but carries none of")
}

// And an exemption for a job that does not exist at all.
func TestExemptingAJobThatIsNotThereFires(t *testing.T) {
	x := of(t, xfs)
	fires(t, x.check(chorefile.CIGate{NonGating: []string{"asan"}}), checkNeedsEveryJob,
		"`asan` is declared non-gating but is not a job in")
}

// A `continue-on-error: true` job inside `needs:` is the quietest hole of the
// lot: the job reports `success` however it ended, so the aggregate reads a
// green tick for a red job, and both halves of the old guard pass.
func TestAContinueOnErrorJobInsideNeedsFires(t *testing.T) {
	p := of(t, partitions).mutate(".github/workflows/ci.yml", "  oracle:\n",
		"  oracle:\n    continue-on-error: true\n")
	fires(t, p.check(chorefile.CIGate{}), checkNeedsEveryJob,
		"`oracle` carries [\"continue-on-error\"] but is not declared non-gating")
}

// ---------------------------------------------------------------------
// The files the gate reads have to be there.
// ---------------------------------------------------------------------

// A guard that cannot read its input has not passed.
func TestAMissingInputFiresRatherThanPassing(t *testing.T) {
	x := of(t, xfs)
	if err := os.Remove(x.path(".github-guard")); err != nil {
		t.Fatal(err)
	}
	fires(t, x.check(chorefile.CIGate{}), checkRequiresOne,
		"nothing in the repository knows what it says")

	p := of(t, partitions)
	if err := os.Remove(p.path(".github/workflows/ci.yml")); err != nil {
		t.Fatal(err)
	}
	fires(t, p.check(chorefile.CIGate{}), checkWorkflowExists, "there is nothing requiring anything")
}

// Unparseable YAML is a failure, never a pass. The line-scanning copies this
// replaces had no way to tell the two apart: a file they could not make sense of
// simply yielded no jobs, and no jobs means nothing to complain about.
func TestAWorkflowThatDoesNotParseFires(t *testing.T) {
	x := of(t, xfs).mutate(".github/workflows/ci.yml", "jobs:\n", "jobs:\n  - [unclosed\n")
	fires(t, x.check(chorefile.CIGate{}), checkWorkflowExists,
		"a file it cannot parse is a failure and never a pass")
}

// An empty workflow is a failure too, for the same reason.
func TestAnEmptyWorkflowFires(t *testing.T) {
	x := of(t, xfs)
	if err := os.WriteFile(x.path(".github/workflows/ci.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fires(t, x.check(chorefile.CIGate{}), checkWorkflowExists, "is empty")
}

// Every failure is reported together. A repository being brought onto the gate
// wants the whole list in one run, not one per push.
func TestEveryFailureIsReportedTogether(t *testing.T) {
	x := of(t, xfs).
		mutate(".github/workflows/ci.yml", "    if: always()\n    "+needsOf[xfs],
			"    needs: [unit, fixtures, test, test-arm64, suite-in-vm]").
		mutate(".github-guard", "\trequired = ci-ok", "\trequired = fmt")
	failures := x.check(chorefile.CIGate{})
	for _, want := range []string{checkNeedsEveryJob, checkRunsWhatever, checkRequiresOne} {
		found := false
		for _, f := range failures {
			if f.Check == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("three separate things are wrong and %q did not report: %v", want, failures)
		}
	}
}

// ---------------------------------------------------------------------
// The shapes YAML allows that a line scan reads wrongly.
// ---------------------------------------------------------------------

// A `run: |` block whose CONTENTS look like a job key, a quoted key, and a flow
// mapping are all ordinary YAML. The eleven line-scanning copies read each of
// them as a job, and a guard that invents jobs reports failures nobody can fix.
func TestYAMLShapesALineScanReadsWrongly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	wf := "" +
		"name: CI\n" +
		"on: [push, pull_request]\n" +
		"jobs:\n" +
		"  \"fmt\":\n" + // a quoted key is the same key
		"    runs-on: ubuntu-latest\n" +
		"    steps:\n" +
		"      - run: |\n" +
		"          echo 'these lines are a shell script, not jobs:'\n" +
		"          echo '  imposter:'\n" +
		"          echo '    if: always()'\n" +
		"  ci-ok: {if: always(), needs: [fmt], runs-on: ubuntu-latest}\n" // a flow mapping
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github-guard"), []byte("[checks]\n\trequired = ci-ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	passes(t, Check(dir, chorefile.CIGate{}), "only `fmt` and `ci-ok` are jobs here")
}

// `required = value # note` is a value with an inline comment, and `required =
// "a # b"` is a value containing one. git-config says so and this reads it the
// same way.
func TestGitConfigValueQuotingAndInlineComments(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"[checks]\n\trequired = ci-ok # the one gate\n", []string{"ci-ok"}},
		{"[checks]\n\trequired = \"ci # ok\"\n", []string{"ci # ok"}},
		{"[CHECKS]\n\tREQUIRED = ci-ok\n", []string{"ci-ok"}},
		{"[other]\n\trequired = ci-ok\n", []string{}},
		{"# required = ci-ok\n[checks]\n\trequired = ci-ok\n", []string{"ci-ok"}},
	} {
		got := requiredChecks(tc.text)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("requiredChecks(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
