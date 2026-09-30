#!/usr/bin/env bash
# install.sh — install a released chore binary, checksum-verified. The body of
# the `install` composite action beside it, kept as a file rather than inline
# YAML so shellcheck can read it and so it can be run by hand to test a change:
#
#     CHORE_VERSION=0.14.2 CHORE_DESTINATION=/tmp/chore-bin \
#         bash .github/actions/install/install.sh
#
# Inputs, from the environment:
#   CHORE_VERSION      release to install, e.g. 0.14.2 (a leading `v` is dropped)
#   CHORE_DESTINATION  directory to install into; default $HOME/.local/bin
#
# Under Actions it also appends the destination to $GITHUB_PATH and writes the
# `version` and `path` outputs to $GITHUB_OUTPUT. Outside Actions those are
# unset, and it only installs.
#
# The asset name is spelled here and in .goreleaser.yml, and nowhere else. The
# two live in the same repository so that changing one without the other is a
# diff a reviewer sees, not a 404 a consumer finds (#52).
set -euo pipefail

# The tag is `v0.14.2`, the asset and `chore --version` say `0.14.2`. Accept
# either spelling: a `v` pasted from the tag would otherwise download
# `vv0.14.2` and fail with a 404 that points nowhere near the cause.
version="${CHORE_VERSION:-}"
version="${version#v}"
if [ -z "$version" ]; then
  echo "::error::no chore version given (the action's \`version\` input, or CHORE_VERSION)" >&2
  exit 1
fi

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *)
    # Fail rather than skip. A silent no-op leaves the NEXT step failing on
    # `chore: command not found`, which names the wrong cause. Git Bash on a
    # Windows runner reports MINGW64_NT-*, and chore publishes no build for it.
    echo "::error::chore publishes linux and darwin builds only, and this runner" \
      "reports $(uname -s). Guard the step with \`if: runner.os != 'Windows'\`" \
      "(a step-level if:, so the job still gates) and do the work another way." >&2
    exit 1
    ;;
esac

# The releases say `x86_64` and `arm64` (see .goreleaser.yml). `uname -m` says
# `arm64` on darwin and `aarch64` on linux for the same CPU, and `amd64` is what
# people type from memory — the hand-written copy that got this wrong is the
# outage in #52.
case "$(uname -m)" in
  x86_64 | amd64) arch=x86_64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *)
    echo "::error::chore publishes no build for $(uname -m)" >&2
    exit 1
    ;;
esac

tarball="chore-${version}-${os}-${arch}.tar.gz"
base="https://github.com/antimatter-studios/chore/releases/download/v${version}"

# A fresh directory per run, so a second install in the same job (another
# version, a retry) cannot pick up the first one's extracted binary.
tmp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
work="$(mktemp -d "${tmp_root%/}/install-chore.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# A 404 is a version that was never released, or a platform it does not ship.
# `curl -f` makes that a non-zero exit instead of a 9-byte "Not Found" body that
# tar then reports as a corrupt archive — an error that names the wrong cause.
curl -fsSL --retry 3 --retry-delay 2 -o "$work/$tarball" "$base/$tarball" || {
  echo "::error::cannot download $tarball from $base — is v${version} released," \
    "and does it publish ${os}-${arch}?" >&2
  exit 1
}
curl -fsSL --retry 3 --retry-delay 2 -o "$work/checksums.txt" "$base/checksums.txt" || {
  echo "::error::cannot download checksums.txt from $base" >&2
  exit 1
}

# Verified rather than trusted: the tarball comes over the network into a job
# that is about to run it. Matched on the whole filename field, not a grep
# pattern, because the dots in a version are regex wildcards.
line="$(awk -v f="$tarball" '$2 == f' "$work/checksums.txt")"
if [ -z "$line" ]; then
  echo "::error::$tarball is not listed in checksums.txt for v${version}" >&2
  exit 1
fi
# macOS has no sha256sum; `shasum -a 256 -c` reads the same format.
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$work" && printf '%s\n' "$line" | sha256sum -c -)
else
  (cd "$work" && printf '%s\n' "$line" | shasum -a 256 -c -)
fi

# The archive is flat (wrap_in_directory: false in .goreleaser.yml), so the
# binary is at the top level. Extract only it; the docs beside it are not needed.
tar -xzf "$work/$tarball" -C "$work" chore || {
  echo "::error::no chore binary at the top level of $tarball" >&2
  exit 1
}

dest="${CHORE_DESTINATION:-$HOME/.local/bin}"
# mkdir THEN install, because BSD install has no -D: on macOS `install -D` is an
# unknown option, not a request to create the parent.
mkdir -p "$dest"
install -m 0755 "$work/chore" "$dest/chore"
dest="$(cd "$dest" && pwd)"

# Ask the installed binary, not the input: the output is then evidence that what
# landed on disk runs and is the release that was asked for.
installed="$("$dest/chore" --version | head -n 1)"
if [ "$installed" != "$version" ]; then
  echo "::error::asked for chore $version, but the installed binary reports '$installed'" >&2
  exit 1
fi

if [ -n "${GITHUB_PATH:-}" ]; then
  echo "$dest" >>"$GITHUB_PATH"
fi
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "version=$installed"
    echo "path=$dest/chore"
  } >>"$GITHUB_OUTPUT"
fi
echo "installed chore $installed at $dest/chore"
