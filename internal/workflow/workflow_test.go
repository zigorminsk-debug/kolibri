package workflow

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var dump = flag.String("dump", "", "write every rendered workflow into this directory (for actionlint)")

func TestRenderAllKinds(t *testing.T) {
	for _, info := range Kinds() {
		if info.Kind == Auto {
			continue
		}
		for _, release := range []bool{true, false} {
			out, err := Render(Options{Kind: info.Kind, Branch: "main", AppName: "my app!", Targets: []string{"linux", "windows", "macos", "linux", "beos"}, Release: release})
			if err != nil {
				t.Fatalf("%s: %v", info.Kind, err)
			}
			if strings.Contains(out, "<%") || strings.Contains(out, "%>") || strings.Contains(out, "<no value>") {
				t.Errorf("%s: unrendered template syntax:\n%s", info.Kind, out)
			}
			if strings.Contains(out, "\t") {
				t.Errorf("%s: tab in YAML", info.Kind)
			}
			if !strings.Contains(out, `branches: ["main"]`) {
				t.Errorf("%s: push trigger for the branch is missing", info.Kind)
			}
			if !strings.Contains(out, "name: my-app-") {
				t.Errorf("%s: artifact name not sanitised:\n%s", info.Kind, out)
			}
			if got := strings.Contains(out, "gh release create"); got != release {
				t.Errorf("%s: release job present = %v, want %v", info.Kind, got, release)
			}
			if release != strings.Contains(out, "contents: write") {
				t.Errorf("%s: contents permission does not match release=%v", info.Kind, release)
			}
			if info.PerOS {
				w, l, m := strings.Index(out, "target: windows"), strings.Index(out, "target: linux"), strings.Index(out, "target: macos")
				if w < 0 || l < w || m < l || strings.Count(out, "target: linux") != 1 {
					t.Errorf("%s: matrix wrong:\n%s", info.Kind, out)
				}
			}
			if *dump != "" {
				name := string(info.Kind)
				if release {
					name += "-release"
				}
				if err := os.WriteFile(filepath.Join(*dump, name+".yml"), []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// The release job must name every artifact it downloads. Asked for "all
// artifacts", download-artifact unpacks a single one straight into the
// folder and several ones into folders of their own — a job written for
// one layout breaks on the other. That is how a project built for Windows
// only lost its release.
func TestReleaseDownloadsEachArtifactByName(t *testing.T) {
	cases := []struct {
		kind    Kind
		targets []string
		want    []string
	}{
		{Go, []string{"windows"}, []string{"app-windows"}},
		{GoFyne, []string{"linux", "windows", "macos"}, []string{"app-windows", "app-linux", "app-macos"}},
		{Static, nil, []string{"app-web"}},
		{Node, []string{"linux"}, []string{"app-web"}},
		{Generic, nil, []string{"app-source"}},
	}
	for _, c := range cases {
		out, err := Render(Options{Kind: c.kind, Branch: "main", AppName: "app", Targets: c.targets, Release: true})
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		_, release, ok := strings.Cut(out, "\n  release:\n")
		if !ok {
			t.Fatalf("%s: no release job:\n%s", c.kind, out)
		}
		if n := strings.Count(release, "uses: actions/download-artifact@"); n != len(c.want) {
			t.Errorf("%s: %d download steps, want %d:\n%s", c.kind, n, len(c.want), release)
		}
		for _, name := range c.want {
			step := "        with:\n          name: " + name + "\n          path: artifacts/" + name + "\n"
			if !strings.Contains(release, step) {
				t.Errorf("%s: release does not download %s into its own folder:\n%s", c.kind, name, release)
			}
			// What is downloaded must be what the build uploads.
			if c.kind == Go || c.kind == GoFyne {
				continue // uploaded as <app>-${{ matrix.target }}
			}
			if !strings.Contains(out, "uses: actions/upload-artifact@v7\n        with:\n          name: "+name+"\n") {
				t.Errorf("%s: the build does not upload %s:\n%s", c.kind, name, out)
			}
		}
		if !strings.Contains(release, "::error title=Release::") {
			t.Errorf("%s: a failed release must explain itself in an annotation", c.kind)
		}
	}
}

// Built on Windows, the templates may arrive with CRLF line endings.
func TestRenderHasNoCarriageReturns(t *testing.T) {
	for name, text := range map[string]string{"part": "    steps:\r\n      - run: a\r\n", "whole": "a: 1\r\n<% template \"p\" . %>\r\nb: 2\r\n"} {
		_ = name
		if strings.Contains(cleanTemplate(text), "\r") {
			t.Errorf("carriage return survived in %q", text)
		}
	}
	out, err := Render(Options{Kind: Go, Branch: "main", AppName: "app", Targets: []string{"windows"}, Release: true})
	if err != nil || strings.Contains(out, "\r") || strings.Contains(out, "\n\n\n") {
		t.Errorf("err = %v, output has carriage returns or doubled blank lines:\n%q", err, out)
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	if _, err := Render(Options{Kind: Go, Branch: "main", AppName: "x"}); err == nil {
		t.Error("no targets: expected an error")
	}
	if _, err := Render(Options{Kind: Go, Branch: "a\"b", AppName: "x", Targets: []string{"linux"}}); err == nil {
		t.Error("quote in branch: expected an error")
	}
	if _, err := Render(Options{Kind: Auto, Branch: "main", Targets: []string{"linux"}}); err == nil {
		t.Error("auto kind: expected an error")
	}
	if _, err := Render(Options{Kind: Static, Branch: "main", AppName: "x"}); err != nil {
		t.Errorf("static site needs no targets: %v", err)
	}
}

func TestDetect(t *testing.T) {
	read := func(files map[string]string) func(string) string {
		return func(p string) string { return files[p] }
	}
	cases := []struct {
		want  Kind
		files map[string]string
	}{
		{Go, map[string]string{"go.mod": "module x", "main.go": "package main"}},
		{GoFyne, map[string]string{"go.mod": "module x", "ui/win.go": `import "fyne.io/fyne/v2/app"`}},
		{GoFyne, map[string]string{"go.mod": "module x\nrequire fyne.io/fyne/v2 v2.5.0", "main.go": ""}},
		{Rust, map[string]string{"Cargo.toml": "", "src/main.rs": ""}},
		{DotNet, map[string]string{"App.csproj": "", "Program.cs": ""}},
		{CMake, map[string]string{"CMakeLists.txt": "", "main.cpp": ""}},
		{Node, map[string]string{"package.json": "", "index.html": ""}},
		{Python, map[string]string{"main.py": ""}},
		{Python, map[string]string{"app.py": "", "requirements.txt": ""}},
		{Static, map[string]string{"index.html": "", "css/a.css": ""}},
		{Generic, map[string]string{"README.md": "", "sub/go.mod": ""}},
	}
	for _, c := range cases {
		var paths []string
		for p := range c.files {
			paths = append(paths, p)
		}
		if got := Detect(paths, read(c.files)); got != c.want {
			t.Errorf("Detect(%v) = %s, want %s", paths, got, c.want)
		}
	}
}

func TestContracts(t *testing.T) {
	for _, info := range Kinds() {
		if info.Kind != Auto && info.Contract == "" {
			t.Errorf("%s has no contract", info.Kind)
		}
	}
	if auto := AutoContract(); !strings.Contains(auto, "Fyne") || !strings.Contains(auto, "main.py") {
		t.Errorf("auto contract misses stacks:\n%s", auto)
	}
}
