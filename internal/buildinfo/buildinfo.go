// Package buildinfo answers "which binary is this, exactly".
//
// The version is LIVE wherever it can be: read out of the build the Go toolchain
// already recorded, rather than hardcoded in source or supplied by whoever ran
// the build. Three cases, in order:
//
//  1. A stamped version wins — `-ldflags "-X main.version=1.2.3"`. The release
//     pipeline builds with `-buildvcs=false` (a recorded commit and timestamp are
//     the only reason two builds of the same source differ, and reproducibility
//     is the point), so a release binary has no VCS data at all and the stamp is
//     the only truth available.
//  2. Otherwise the module version from `go install …@v0.1.1`, which the toolchain
//     records for anything installed from a module proxy.
//  3. Otherwise the VCS revision of the tree it was built from — `dev+<sha>`, and
//     `-dirty` when that tree had uncommitted changes. This is what a plain
//     `go build` in a checkout reports, with nothing passed to it.
//
// So a local build identifies itself without the build command having to work out
// its own commit, which is the part that used to be hardcoded.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Info is everything worth knowing about the running binary.
type Info struct {
	// Date is when the SOURCE was committed, not when someone compiled it.
	//
	// A wall-clock build time would break the one property the release pipeline
	// proves on every tag: rebuild the same source and get the same bytes. A
	// timestamp is precisely what makes two builds differ, which is why the release
	// passes -buildvcs=false. The commit date is a property of the source, so it is
	// stable across rebuilds — and it answers the useful question ("how old is this
	// code") rather than the accidental one ("when did a machine run go build").
	Date     string
	Version  string // "1.2.3", "dev+a581449-dirty", or "dev" if nothing is known
	Commit   string // short revision, when the build recorded one
	Dirty    bool   // the working tree had uncommitted changes
	Go       string // toolchain that compiled it, e.g. "go1.25.0"
	Platform string // "darwin/arm64"
	Dev      bool   // not a released version — a banner is worth printing
}

// Get resolves the running binary's identity. stamped is main.version, which is
// "dev" (never empty) unless the build passed -X main.version.
func Get(stamped, stampedDate string) Info {
	i := Info{
		Version:  stamped,
		Date:     stampedDate,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	bi, ok := debug.ReadBuildInfo()
	if ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				i.Commit = short(s.Value)
			case "vcs.modified":
				i.Dirty = s.Value == "true"
			case "vcs.time":
				// The checkout's commit date, recorded by any build that did not
				// pass -buildvcs=false. A stamp still wins: a release has no VCS
				// data at all, so the stamp is the only source there.
				if i.Date == "" {
					i.Date = s.Value
				}
			}
		}
	}

	// A stamped version is authoritative — it is what a release says it is.
	if stamped != "" && stamped != "dev" {
		i.Dev = strings.HasPrefix(stamped, "dev")
		// A dev stamp may already carry the revision (dev+abc1234); do not repeat
		// it in the detail block.
		if i.Commit == "" {
			if _, rev, found := strings.Cut(stamped, "+"); found {
				i.Commit = strings.TrimSuffix(rev, "-dirty")
				i.Dirty = i.Dirty || strings.HasSuffix(rev, "-dirty")
			}
		}
		return i
	}

	i.Dev = true

	// The recorded REVISION comes first, deliberately. Since Go 1.24 a plain
	// `go build` in a tagged checkout also stamps Main.Version — as a
	// pseudo-version like "0.1.2-0.20260727182255-8e37399a032b+dirty", which is
	// accurate and unreadable, and worse, it names the NEXT version rather than
	// what this tree is. `dev+8e37399-dirty` says the same thing usefully.
	if i.Commit != "" {
		i.Version = "dev+" + i.Commit
		if i.Dirty {
			i.Version += "-dirty"
		}
		return i
	}

	if ok {
		// No VCS data: either a module download (`go install …@v0.1.1`, where the
		// proxy has no repository) or a build with -buildvcs=false. Main.Version is
		// then the only source, and only a clean tag is worth showing as a version.
		if v := releaseTag(bi.Main.Version); v != "" {
			i.Version, i.Dev = v, false
			return i
		}
		// A pseudo-version still carries the revision it was built from; recover it
		// rather than reporting a bare "dev".
		if rev := pseudoRevision(bi.Main.Version); rev != "" {
			i.Commit = rev
			i.Version = "dev+" + rev
			return i
		}
	}
	return i
}

// releaseTag returns the version for a module built from a plain semver tag —
// "v0.1.1" → "0.1.1" — and "" for anything else. A pseudo-version, a +dirty
// suffix or "(devel)" are all "not a release".
func releaseTag(v string) string {
	if v == "" || v == "(devel)" || strings.ContainsAny(v, "+") {
		return ""
	}
	v = strings.TrimPrefix(v, "v")
	// A release tag is digits and dots only; a pseudo-version has the build
	// timestamp and revision hung off a dash.
	if strings.Contains(v, "-") {
		return ""
	}
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			return ""
		}
	}
	return v
}

// pseudoRevision digs the revision out of a Go pseudo-version, whose last
// dash-separated field is a 12-character prefix of the commit hash:
//
//	0.1.2-0.20260727182255-8e37399a032b+dirty  →  8e37399
func pseudoRevision(v string) string {
	v, _, _ = strings.Cut(v, "+") // drop a +dirty suffix
	i := strings.LastIndex(v, "-")
	if i < 0 {
		return ""
	}
	return short(v[i+1:])
}

// short trims a full git revision to the 7 characters everyone actually reads.
func short(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
