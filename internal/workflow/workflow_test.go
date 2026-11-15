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
