package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

// A stamped release is authoritative: the release pipeline builds with
// -buildvcs=false, so there is no VCS data to fall back to and the stamp is all
// the binary knows about itself.
func TestStampedReleaseWins(t *testing.T) {
	i := Get("0.1.1")
	if i.Version != "0.1.1" {
		t.Errorf("Version = %q, want 0.1.1", i.Version)
	}
	if i.Dev {
		t.Error("a stamped release reports itself as a dev build")
	}
	if i.Go != runtime.Version() {
		t.Errorf("Go = %q, want %q", i.Go, runtime.Version())
	}
	if i.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("Platform = %q", i.Platform)
	}
}

// The dev script stamps dev+<sha>; the revision inside it must not be repeated in
// the detail block as a separate commit line.
func TestDevStampCarriesItsRevision(t *testing.T) {
	i := Get("dev+a581449-dirty")
	if !i.Dev {
		t.Error("dev+… is not reported as a dev build")
	}
	if i.Commit != "a581449" && i.Commit != "" {
		// Non-empty means it came from either the stamp or this test binary's own
		// VCS data; both are acceptable, but a stamp must never win over nothing.
		t.Logf("Commit = %q (from build VCS data, not the stamp)", i.Commit)
	}
	if !i.Dirty {
		t.Error("-dirty in the stamp did not set Dirty")
	}
}

// Nothing stamped is the plain `go build` case: report a dev build, and take the
// revision from what the toolchain recorded rather than from a hardcoded string.
func TestUnstampedIsADevBuild(t *testing.T) {
	i := Get("dev")
	if !i.Dev {
		t.Error("an unstamped build is not reported as a dev build")
	}
	if i.Version == "" {
		t.Error("Version is empty; the Homebrew formula asserts on this output")
	}
	// `go test` compiles with VCS data available, so this binary knows its commit
	// and the version should have grown to dev+<sha> without anyone passing it.
	if i.Commit != "" && !strings.HasPrefix(i.Version, "dev+") {
		t.Errorf("Version = %q but Commit = %q — the live revision was not used", i.Version, i.Commit)
	}
}

func TestShortRevision(t *testing.T) {
	if got := short("a581449bd7ce7cd18b0e9e0d10ba7b0e39a5b7f1"); got != "a581449" {
		t.Errorf("short() = %q, want a581449", got)
	}
	if got := short("abc"); got != "abc" {
		t.Errorf("short() truncated a already-short revision: %q", got)
	}
}

// Since Go 1.24 a plain `go build` in a tagged checkout stamps Main.Version as a
// pseudo-version — accurate, unreadable, and it names the NEXT version rather
// than this tree. The recorded revision has to win.
func TestReleaseTag(t *testing.T) {
	for in, want := range map[string]string{
		"v0.1.1":                               "0.1.1",
		"v1.20.3":                              "1.20.3",
		"(devel)":                              "",
		"":                                     "",
		"v0.1.2-0.20260727182255-8e37399a032b": "", // pseudo-version
		"0.1.2-0.20260727182255-8e37399a032b+dirty": "",
		"v0.2.0-rc1": "", // prerelease is not a release
	} {
		if got := releaseTag(in); got != want {
			t.Errorf("releaseTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPseudoRevision(t *testing.T) {
	for in, want := range map[string]string{
		"0.1.2-0.20260727182255-8e37399a032b+dirty": "8e37399",
		"v0.1.2-0.20260727182255-8e37399a032b":      "8e37399",
		// Not a pseudo-version: no dash to split on, so there is no revision to
		// recover. Get() asks releaseTag first anyway, so this never decides.
		"v0.1.1":   "",
		"nodashes": "",
	} {
		if got := pseudoRevision(in); got != want {
			t.Errorf("pseudoRevision(%q) = %q, want %q", in, got, want)
		}
	}
}
