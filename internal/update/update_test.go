package update

import (
	"testing"

	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
)

func rel(tag string, pre bool, assets ...string) ghapi.Release {
	r := ghapi.Release{TagName: tag, Prerelease: pre, HTMLURL: "https://example.test/" + tag}
	for _, a := range assets {
		r.Assets = append(r.Assets, ghapi.ReleaseAsset{Name: a, URL: "https://example.test/dl/" + a})
	}
	return r
}

func TestPick(t *testing.T) {
	list := []ghapi.Release{
		rel("build-61", true), rel("v1.1.0", false), rel("build-58", true), rel("v1.2.0-rc1", true),
		rel("v1.0.0", false), rel("build-40", true), rel("nightly", true), rel("build-x", true),
		{TagName: "v9.0.0", Draft: true},
	}
	cases := []struct {
		name   string
		cur    Current
		builds bool
		want   string // tag, "" = up to date
		title  string
	}{
		{"older version gets the newest tagged one", Current{"1.0.0", 30}, true, "v1.1.0", "1.1.0"},
		{"a tagged version wins over a newer build", Current{"1.0.0", 30}, false, "v1.1.0", "1.1.0"},
		{"same version, older build: the newest build", Current{"1.1.0", 59}, true, "build-61", "сборка 61"},
		{"builds not wanted", Current{"1.1.0", 59}, false, "", ""},
		{"up to date", Current{"1.1.0", 61}, true, "", ""},
		{"newer than everything", Current{"2.0.0", 99}, true, "", ""},
		{"pre-release versions and drafts are skipped", Current{"1.1.0", 70}, true, "", ""},
		{"two-part version", Current{"1.0", 61}, true, "v1.1.0", "1.1.0"},
	}
	for _, c := range cases {
		got, ok := Pick(list, c.cur, c.builds)
		if ok != (c.want != "") || got.Release.TagName != c.want || got.Title != c.title {
			t.Errorf("%s: got %q %q ok=%v, want %q %q", c.name, got.Release.TagName, got.Title, ok, c.want, c.title)
		}
		if ok && got.Build != (c.want == "build-61") {
			t.Errorf("%s: Build = %v", c.name, got.Build)
		}
	}
	if _, ok := Pick(nil, Current{"1.0.0", 1}, true); ok {
		t.Error("no releases: nothing to offer")
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][3]int{"v1.2.3": {1, 2, 3}, "1.2": {1, 2, 0}, "v2": {2, 0, 0}, "1.2.3-rc1": {1, 2, 3}, " v1.0.10 ": {1, 0, 10}} {
		if got, ok := parseVersion(in); !ok || got != want {
			t.Errorf("parseVersion(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "v", "build-5", "1.2.3.4", "1.x", "-1"} {
		if _, ok := parseVersion(in); ok {
			t.Errorf("parseVersion(%q) must fail", in)
		}
	}
	if compare([3]int{1, 10, 0}, [3]int{1, 9, 9}) <= 0 || compare([3]int{1, 0, 0}, [3]int{1, 0, 0}) != 0 || compare([3]int{0, 9, 0}, [3]int{1, 0, 0}) >= 0 {
		t.Error("compare is wrong")
	}
}

func TestAsset(t *testing.T) {
	r := rel("v1.1.0", false, "KolibriForge-Setup.exe", "KolibriForge-windows-amd64.zip", "KolibriForge-linux-amd64.tar.gz", "KolibriForge-macos-arm64.zip")
	for goos, want := range map[string]string{"windows": "KolibriForge-Setup.exe", "linux": "KolibriForge-linux-amd64.tar.gz", "darwin": "KolibriForge-macos-arm64.zip"} {
		if got, ok := Asset(r, goos); !ok || got.Name != want {
			t.Errorf("%s: %q %v, want %q", goos, got.Name, ok, want)
		}
	}
	if _, ok := Asset(r, "freebsd"); ok {
		t.Error("no file for an unknown system")
	}
	if _, ok := Asset(rel("v1.1.0", false), "windows"); ok {
		t.Error("a release without files has nothing to download")
	}
}

func TestKnown(t *testing.T) {
	if (Current{}).Known() || !(Current{Version: "1.0.0"}).Known() {
		t.Error("Known is wrong")
	}
}
