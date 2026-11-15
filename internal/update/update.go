// Package update decides whether a release published on GitHub is newer
// than the running program.
//
// The project publishes two kinds of releases from one workflow: a tag
// vX.Y.Z gives a full release, every push to main gives a pre-release
// "build-N", where N is the number of the workflow run. The same N is
// stamped into the program as its build number, so builds can be compared
// across both kinds; versions are compared only between tagged releases.
package update

import (
	"strconv"
	"strings"

	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
)

// Owner and Repo say where the program is published.
const (
	Owner = "zigorminsk-debug"
	Repo  = "kolibri"
)

// Current describes the running program.
type Current struct {
	Version string // "1.2.0"; empty when built from source without packaging
	Build   int    // number of the workflow run that built it; 0 when unknown
}

// Known reports whether the program knows what it is: a build made from
// source without the packaging step cannot be compared with releases.
func (c Current) Known() bool { return c.Version != "" }

// Found is the release to offer.
type Found struct {
	Release ghapi.Release
	// Title is how to name it for the user: "1.2.0" or "сборка 57".
	Title string
	// Build is true for a build-N pre-release.
	Build bool
}

// Pick chooses the release to offer from a list: the highest tagged version
// above the current one; when there is none and builds are wanted, the
// highest build-N above the current build. ok is false when the program is
// up to date.
func Pick(releases []ghapi.Release, cur Current, builds bool) (found Found, ok bool) {
	curVersion, _ := parseVersion(cur.Version)
	var best ghapi.Release
	bestVersion, haveVersion := curVersion, false
	bestBuild, haveBuild := cur.Build, false
	var bestBuildRelease ghapi.Release
	for _, r := range releases {
		if r.Draft {
			continue
		}
		if n, isBuild := parseBuild(r.TagName); isBuild {
			if n > bestBuild {
				bestBuild, bestBuildRelease, haveBuild = n, r, true
			}
			continue
		}
		v, isVersion := parseVersion(r.TagName)
		if !isVersion || r.Prerelease {
			continue
		}
		if compare(v, bestVersion) > 0 {
			bestVersion, best, haveVersion = v, r, true
		}
	}
	switch {
	case haveVersion:
		return Found{Release: best, Title: strings.TrimPrefix(best.TagName, "v")}, true
	case haveBuild && builds:
		return Found{Release: bestBuildRelease, Title: "сборка " + strconv.Itoa(bestBuild), Build: true}, true
	}
	return Found{}, false
}

// Asset picks the file to download for an operating system (a GOOS value);
// ok is false when the release has no such file and the user should be sent
// to the release page instead.
func Asset(r ghapi.Release, goos string) (asset ghapi.ReleaseAsset, ok bool) {
	want := func(name string) bool { return false }
	switch goos {
	case "windows":
		want = func(name string) bool { return strings.HasSuffix(name, "-setup.exe") }
	case "linux":
		want = func(name string) bool { return strings.Contains(name, "linux") && strings.HasSuffix(name, ".tar.gz") }
	case "darwin":
		want = func(name string) bool { return strings.Contains(name, "macos") && strings.HasSuffix(name, ".zip") }
	}
	for _, a := range r.Assets {
		if a.URL != "" && want(strings.ToLower(a.Name)) {
			return a, true
		}
	}
	return ghapi.ReleaseAsset{}, false
}

func parseBuild(tag string) (int, bool) {
	rest, ok := strings.CutPrefix(tag, "build-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

// parseVersion reads "v1.2.3", "1.2" or "1.2.3-rc1" (the suffix is ignored).
func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if s == "" || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func compare(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}
