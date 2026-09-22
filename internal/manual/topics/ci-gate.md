<!-- Generated from `chore:manual` comments. Do not edit; run `chore manual`. -->
<!-- sources: internal/cli/cli.go:1199 -->
---
title: The CI gate
summary: one required check, and it stands for every job
aliases: ci gate
---

# The CI gate

    chore ci:gate

Branch protection names checks, and a check is a job name. Naming each job
means the list has to be edited whenever one is renamed, split into a matrix
leg, or added — and until someone does, the new work is required by nobody.
The opposite spelling is worse: a required check no job produces reads as
permanently pending, and with `enforce_admins` on nothing merges and there is
no failure to point at.

So protection names ONE job — `ci-ok` — which `needs:` every gating job and
fails unless each of them concluded success. `chore ci:gate` is what keeps
that true, and it reports every failure at once: a repository being brought
onto the gate wants the whole list in one run rather than one per push.

## What it checks

- **the gate workflow runs on `pull_request`.** A `release.yml` on a tag fires
  after the merge it would be gating, and a nightly-cron `fuzz.yml` never sees
  a pull request at all. Requiring a check from either blocks every merge
  forever, with nothing to point at.
- **the aggregate `needs:` every other gating job**, and `needs:` does not name
  a job that does not exist — GitHub refuses to run a workflow with an
  unresolvable `needs:`, so the one required check never reports.
- **the aggregate carries `if: always()`.** `${{ always() }}` is the same
  expression and passes; `always() && github.event_name == 'pull_request'` does
  not, because a condition that can be false is a condition under which the one
  required check does not report.
- **`.github-guard` requires the aggregate and nothing else.** It is git-config
  format, and a `required =` inside a comment is not a requirement.
- **a job carrying `if:` or `continue-on-error:`** must be BOTH declared
  non-gating AND left out of `needs:`, and the reverse: an exemption for a job
  that does not exist is an exemption waiting to silently cover a future job of
  that name.

## Configuring it

Nothing to write for a repository that answers the defaults —
`.github/workflows/ci.yml`, an aggregate called `ci-ok`, `.github-guard`, and
no exemptions. For the rest:

    ci_gate:
      workflow: .github/workflows/ci.yml
      aggregate: ci-ok
      guard: .github-guard
      non_gating: [asan]

**There are no flags for these, deliberately.** The gate's verdict has to be
the one CI got, and a flag is what a hand-typed run omits: `chore ci:gate`
with the exemption forgotten reports a clean gate on a repository whose
exemption it never read.

`non_gating:` is checked against the workflow in both directions. A job named
here that carries neither key fails — exempting a job that runs
unconditionally takes a working gate off a job whose failures are real.
