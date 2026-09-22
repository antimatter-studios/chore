// Package cigate checks that one required check gates a pull request, and that
// it stands for every job.
//
// # The rule
//
// Branch protection names checks, and a check is a job name. Naming each job
// means the list has to be edited whenever a job is renamed, split into a matrix
// leg, or added — and until someone does, the new work is required by nobody.
// The opposite spelling is worse: a required check no job produces reads as
// permanently pending, and with `enforce_admins` on nothing merges and there is
// no failure to point at.
//
// So protection names one job — `ci-ok` — which `needs:` every gating job and
// fails unless each concluded success. This package is what keeps that true.
//
// # Why this lives in a task runner and not in a test suite
//
// It was a Rust crate, `am-ci-guard`, taken as a `[dev-dependencies]` entry and
// called from `tests/ci_aggregate_gate.rs` (rust-fs-core#156/#157). It worked.
// It was still the wrong container, twice over:
//
//   - It tests no driver functionality. It parses a YAML file, reads a config
//     file, and compares strings — sitting in `tests/` beside tests that read
//     superblocks and walk extent trees. Worse, `ci-ok` enforces per-suite
//     executed-test floors, so meta-tests inflate the very counts a repository
//     uses to satisfy its own gate.
//   - A crate drags a pure CI concern into the cargo dependency graph:
//     crates.io publishing, version pins, and entanglement with whichever
//     release deadlock the family is in this month. None of that has anything
//     to do with checking that a YAML file agrees with a config file.
//
// Half that crate's 651 lines were hand-rolled untyped-YAML tree walking and a
// builder API, needed only because the parser returned an untyped enum. With a
// typed struct and one Unmarshal both disappear.
//
// # What gates, and what must not
//
// Only a workflow that runs on `pull_request` can gate a pull request. A
// `release.yml` on a `v*.*.*` tag fires after the merge it would be gating has
// already happened, and a `fuzz.yml` on `workflow_dispatch` plus a nightly cron
// never sees a pull request at all. Requiring a check from either is requiring a
// check that never reports, which GitHub reads as permanently pending — a
// permanent block on every merge, with nothing to point at.
//
// Within that workflow, a job carrying a job-level `if:` or `continue-on-error:`
// is declaring that it does not gate, and both keys break the aggregate if it
// needs them anyway:
//
//   - `continue-on-error: true` makes the job's `result` `success` even when it
//     failed, so the aggregate reads a green tick for a red job. The gate is on,
//     and it is measuring nothing.
//   - a job-level `if:` that is false on a pull request leaves the job
//     `skipped`, the aggregate counts `skipped` as not-success, and every pull
//     request fails on a job that was never meant to run.
//
// Either way the job must be declared non-gating and left out of `needs:`. This
// is rust-fs-ntfs's rule — its `tests/ci_profile.rs` pins the same two keys and
// its `.github-guard` argues the case at length in its #158/#278 — generalised
// so the declaration is checked against the YAML in BOTH directions. A job
// declared non-gating must actually carry one of those keys, and a job that
// carries one must be declared. Neither list can drift away from the other while
// this passes.
package cigate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/antimatter-studios/chore/internal/chorefile"
	"gopkg.in/yaml.v3"
)

// NonGatingKeys are the job-level keys by which a job declares it does not gate.
//
// Named rather than inlined because the set is the shared decision, not an
// implementation detail: rust-fs-ntfs pins the same two and argues why.
var NonGatingKeys = []string{"if", "continue-on-error"}

// The five checks, named as the tests they replace, so a failure here reads the
// same as the failure the eleven copied test files used to print.
const (
	checkWorkflowExists = "the_gate_workflow_exists"
	checkRunsOnPR       = "the_gate_workflow_runs_on_pull_request"
	checkNeedsEveryJob  = "the_aggregate_job_needs_every_other_job"
	checkRunsWhatever   = "the_aggregate_runs_whatever_happened"
	checkRequiresOne    = "protection_requires_the_aggregate_and_nothing_else"
)

// Failure is everything one check can conclude.
type Failure struct {
	// Check that failed, named as the test it replaces.
	Check string
	// Detail says what is wrong and what it costs — written to be read in a CI log.
	Detail string
}

func (f Failure) String() string { return f.Check + ": " + f.Detail }

// Defaults fills in what a repository did not say. Every repository in this
// family answers these the same way, which is why the block is optional — and
// why there is no flag for any of them: configuration that arrives on the
// command line is configuration a hand-typed run silently omits, and then the
// answer someone gets at a prompt is not the answer CI got.
func Defaults(c chorefile.CIGate) chorefile.CIGate {
	if c.Workflow == "" {
		c.Workflow = ".github/workflows/ci.yml"
	}
	if c.Aggregate == "" {
		c.Aggregate = "ci-ok"
	}
	if c.Guard == "" {
		c.Guard = ".github-guard"
	}
	return c
}

// Check runs every check against the repository rooted at repo and returns what
// failed — all of it, not the first: a repository being brought onto the gate
// wants the whole list in one run rather than one per push.
func Check(repo string, cfg chorefile.CIGate) []Failure {
	cfg = Defaults(cfg)

	text, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(cfg.Workflow)))
	if err != nil {
		return []Failure{{checkWorkflowExists, fmt.Sprintf(
			"cannot read %s: %v. The gate is this workflow; without it there is "+
				"nothing requiring anything.", cfg.Workflow, err)}}
	}
	var doc workflow
	// Parsed as YAML rather than scanned as lines. A quoted key, a flow mapping,
	// and a `run: |` block whose CONTENTS look like a job key are all ordinary
	// YAML that a line scan reads wrongly — and a guard that misreads its input
	// reports protection it is not providing. yaml.v3 also keeps GitHub's `on:`
	// as the string `on` rather than folding it into a boolean, which is the one
	// key that tells a gating workflow from a release one.
	if err := yaml.Unmarshal(text, &doc); err != nil {
		return []Failure{{checkWorkflowExists, fmt.Sprintf(
			"%s is not valid YAML: %v. This guard reads the workflow rather than "+
				"scanning its text, so a file it cannot parse is a failure and never "+
				"a pass.", cfg.Workflow, err)}}
	}
	if doc.On.Kind == 0 && doc.Jobs.Kind == 0 {
		return []Failure{{checkWorkflowExists, cfg.Workflow + " is empty"}}
	}

	jobs := doc.jobs()
	var out []Failure
	out = append(out, runsOnPullRequest(cfg, doc)...)
	out = append(out, needsEveryGatingJob(cfg, jobs)...)
	out = append(out, runsWhateverHappened(cfg, jobs)...)
	out = append(out, requiresTheAggregateAlone(repo, cfg)...)
	return out
}

// runsOnPullRequest: only a `pull_request` workflow can gate a pull request.
func runsOnPullRequest(cfg chorefile.CIGate, doc workflow) []Failure {
	if doc.On.Kind == 0 {
		return []Failure{{checkRunsOnPR, fmt.Sprintf(
			"%s has no `on:` trigger at all, so it runs for nothing and gates nothing",
			cfg.Workflow)}}
	}
	triggers := names(doc.On)
	if slices.Contains(triggers, "pull_request") {
		return nil
	}
	return []Failure{{checkRunsOnPR, fmt.Sprintf(
		"%s does not run on `pull_request` (it runs on %q), so no check it produces "+
			"ever reports on one. Requiring one would be requiring a check that never "+
			"arrives, which GitHub reads as permanently pending -- with enforce_admins "+
			"on that blocks every merge and there is no failure to point at.",
		cfg.Workflow, triggers)}}
}

// needsEveryGatingJob: every job that gates is in the aggregate's `needs:`, and
// every job that is not, says so.
func needsEveryGatingJob(cfg chorefile.CIGate, jobs []job) []Failure {
	agg := find(jobs, cfg.Aggregate)
	if agg == nil {
		// A repository with no aggregate at all does not quietly pass for want of
		// anything to complain about. rust-fs-ntfs is the real case: five named
		// check names and an argument, at its #278, that the aggregate is the
		// better end state it has not adopted yet.
		return []Failure{{checkNeedsEveryJob, fmt.Sprintf(
			"%s has no `%s` job, so protection has to name every job by hand -- and "+
				"that list drifts the moment one is renamed, split or added. Jobs "+
				"present: %q", cfg.Workflow, cfg.Aggregate, ids(jobs))}}
	}
	needs := agg.Needs
	var out []Failure

	// A declared non-gating job that carries neither key: the list and the
	// workflow have drifted apart, and the job is silently exempt.
	for _, declared := range cfg.NonGating {
		switch j := find(jobs, declared); {
		case j == nil:
			out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
				"`%s` is declared non-gating but is not a job in %s. An exemption for a "+
					"job that does not exist is an exemption waiting to silently cover a "+
					"future job of that name.", declared, cfg.Workflow)})
		case len(j.NonGatingKeys) == 0:
			out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
				"`%s` is declared non-gating but carries none of %q in %s. It runs "+
					"unconditionally and its failure is a real failure, so exempting it "+
					"from `%s`'s `needs:` takes a working gate off a working job.",
				declared, NonGatingKeys, cfg.Workflow, cfg.Aggregate)})
		}
	}

	for _, j := range jobs {
		if j.ID == cfg.Aggregate {
			continue
		}
		declared, inNeeds := slices.Contains(cfg.NonGating, j.ID), slices.Contains(needs, j.ID)
		switch {
		// An ordinary gating job missing from `needs:` gates nothing.
		case len(j.NonGatingKeys) == 0 && !declared && !inNeeds:
			out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
				"`%s` does not need `%s`, so that job gates nothing: it can go red and "+
					"the merge still goes through. `%s` needs %q",
				cfg.Aggregate, j.ID, cfg.Aggregate, needs)})
		// A non-gating job inside `needs:`: whichever key it carries, this breaks.
		case len(j.NonGatingKeys) > 0 && declared && inNeeds:
			out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
				"`%s` needs `%s`, which carries %q and is declared non-gating. "+
					"`continue-on-error` reports `success` for a failed job, so the "+
					"aggregate reads green for red; a job-level `if:` that is false leaves "+
					"it `skipped`, which the aggregate counts as not-success and every "+
					"pull request fails on a job that was never meant to run. Take it out "+
					"of `needs:`.", cfg.Aggregate, j.ID, j.NonGatingKeys)})
		// Carries a non-gating key but was never declared.
		case len(j.NonGatingKeys) > 0 && !declared:
			where := "It is already out of `needs:`, so it gates nothing -- but nothing " +
				"in the repository says that was intended."
			if inNeeds {
				where = fmt.Sprintf(
					"It is in `%s`'s `needs:`, where that key is a hole: a "+
						"`continue-on-error` job reports success however it ended, and a job "+
						"skipped by its `if:` fails the aggregate on every pull request.",
					cfg.Aggregate)
			}
			out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
				"`%s` carries %q but is not declared non-gating. %s Name it under "+
					"`ci_gate: non_gating:` in chores.yml and take it out of `%s`'s "+
					"`needs:`, or take the key off the job.",
				j.ID, j.NonGatingKeys, where, cfg.Aggregate)})
		}
		// Every other shape is either correct or already reported by the loop
		// above; saying it twice helps nobody.
	}

	// `needs:` naming a job that does not exist is a workflow GitHub refuses to
	// run at all -- so the gate never reports, and a required check that never
	// reports blocks every merge.
	for _, need := range needs {
		if find(jobs, need) == nil {
			out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
				"`%s` needs `%s`, which is not a job in %s. GitHub refuses to run a "+
					"workflow with an unresolvable `needs:`, so the one required check "+
					"never reports and nothing merges.", cfg.Aggregate, need, cfg.Workflow)})
		}
	}

	if len(needs) == 0 {
		out = append(out, Failure{checkNeedsEveryJob, fmt.Sprintf(
			"`%s` needs nothing, so it says every job succeeded while asking none of them",
			cfg.Aggregate)})
	}
	return out
}

// runsWhateverHappened: the aggregate has to run even when what it watches did not.
func runsWhateverHappened(cfg chorefile.CIGate, jobs []job) []Failure {
	agg := find(jobs, cfg.Aggregate)
	if agg == nil {
		return nil // already reported, by the check that looks for it
	}
	// `always()` and `${{ always() }}` are the same expression; GitHub accepts a
	// job-level `if:` with or without the braces. Anything else -- `always() &&
	// github.event_name == 'pull_request'`, say -- is a condition that can be
	// FALSE, and then the aggregate is skipped along with everything it was
	// watching. The eleven copies this replaces compared against the literal text
	// `if: always()` and so accepted exactly that narrowing.
	cond := strings.TrimSpace(agg.Condition)
	bare := cond
	if inner, ok := strings.CutPrefix(bare, "${{"); ok {
		if inner, ok := strings.CutSuffix(inner, "}}"); ok {
			bare = strings.TrimSpace(inner)
		}
	}
	if bare == "always()" {
		return nil
	}
	if cond == "" {
		return []Failure{{checkRunsWhatever, fmt.Sprintf(
			"`%s` does not carry `if: always()`, so a cancelled or skipped job leaves "+
				"it skipped too -- and a skipped required check never reports. A job that "+
				"was skipped is not a job that passed, and an aggregate that only runs on "+
				"success cannot say so.", cfg.Aggregate)}}
	}
	return []Failure{{checkRunsWhatever, fmt.Sprintf(
		"`%s` carries `if: %s` rather than `if: always()`. Any condition that can be "+
			"false is a condition under which the one required check does not report, "+
			"and a required check that does not report reads as permanently pending.",
		cfg.Aggregate, cond)}}
}

// requiresTheAggregateAlone: `.github-guard` requires the aggregate, and nothing else.
func requiresTheAggregateAlone(repo string, cfg chorefile.CIGate) []Failure {
	text, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(cfg.Guard)))
	if err != nil {
		return []Failure{{checkRequiresOne, fmt.Sprintf(
			"cannot read %s: %v. Without it the required list is whatever was last "+
				"typed into the settings page, and nothing in the repository knows what "+
				"it says.", cfg.Guard, err)}}
	}
	required := requiredChecks(string(text))
	if len(required) == 1 && required[0] == cfg.Aggregate {
		return nil
	}
	return []Failure{{checkRequiresOne, fmt.Sprintf(
		"%s requires %q; it should name `%s` alone. A job named here as well drifts "+
			"the moment it is renamed, and one named here but not produced by the gate "+
			"workflow is a required check that never reports -- permanently pending, "+
			"blocking every merge with nothing to point at.",
		cfg.Guard, required, cfg.Aggregate)}}
}

// requiredChecks reads the checks `.github-guard` declares as required.
//
// `.github-guard` is git-config format, so `git config -f .github-guard --get-all
// checks.required` is the reference reading. This is that reading, and the part
// that matters is what it does NOT count: a comment. Every one of these files
// opens with a long argued rationale, and a scanner that takes any line
// containing `required =` reads a sentence about what USED to be required as a
// thing that is required.
func requiredChecks(text string) []string {
	section := ""
	out := []string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section != "checks" || !strings.EqualFold(strings.TrimSpace(key), "required") {
			continue
		}
		// git-config strips an inline comment from an unquoted value, and the
		// surrounding quotes from a quoted one.
		value = strings.TrimSpace(value)
		if rest, ok := strings.CutPrefix(value, `"`); ok {
			value, _, _ = strings.Cut(rest, `"`)
		} else if i := strings.IndexAny(value, "#;"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// workflow is the workflow file, reduced to what the gate asks about.
//
// `on:` and `jobs:` stay yaml.Node because both are polymorphic: `on:` is a
// string, a list or a mapping, and `jobs:` has to keep its DOCUMENT ORDER so
// failures come out in the order someone reading the file would meet them.
type workflow struct {
	On   yaml.Node `yaml:"on"`
	Jobs yaml.Node `yaml:"jobs"`
}

// jobBody is one job's mapping, reduced the same way. Every field is a
// yaml.Node so that PRESENCE is what is read: a bare `if: true` is a boolean, a
// typed string field would refuse it, and that is exactly the job this must not
// read as carrying no condition.
type jobBody struct {
	Needs           yaml.Node `yaml:"needs"`
	If              yaml.Node `yaml:"if"`
	ContinueOnError yaml.Node `yaml:"continue-on-error"`
}

// job is one job in the workflow, as the gate sees it.
type job struct {
	ID            string
	Needs         []string
	Condition     string
	NonGatingKeys []string
}

func (w workflow) jobs() []job {
	var out []job
	c := w.Jobs.Content
	for i := 0; i+1 < len(c); i += 2 {
		var body jobBody
		// A job body that is not a mapping decodes to nothing rather than being
		// dropped: a job the gate cannot read still has to show up as a job that
		// gates nothing.
		_ = c[i+1].Decode(&body)
		j := job{ID: c[i].Value, Needs: names(body.Needs), Condition: body.If.Value}
		if body.If.Kind != 0 {
			j.NonGatingKeys = append(j.NonGatingKeys, "if")
		}
		if body.ContinueOnError.Kind != 0 {
			j.NonGatingKeys = append(j.NonGatingKeys, "continue-on-error")
		}
		out = append(out, j)
	}
	return out
}

// names reads a scalar, every scalar in a sequence, or every key of a mapping.
//
// `on: pull_request`, `on: [push, pull_request]` and `on:` with a nested mapping
// are the three spellings GitHub accepts and they mean the same thing to this
// question; `needs: a` and `needs: [a, b]` are the two it accepts there.
func names(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	case yaml.MappingNode:
		out := make([]string, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
		return out
	}
	return nil
}

func find(jobs []job, id string) *job {
	for i := range jobs {
		if jobs[i].ID == id {
			return &jobs[i]
		}
	}
	return nil
}

func ids(jobs []job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}
