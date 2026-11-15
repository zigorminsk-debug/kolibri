// Package workflow owns the GitHub Actions side of the pipeline: it decides
// what kind of project a set of files is, renders the build workflow for it,
// and tells the model which conventions that workflow relies on.
package workflow

import (
	"bytes"
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"text/template"
)

// Path is where the managed workflow lives in the target repository.
const Path = ".github/workflows/kolibri-build.yml"

// Kind is a supported project type.
type Kind string

const (
	Auto    Kind = "auto"
	Go      Kind = "go"
	GoFyne  Kind = "go-fyne"
	Python  Kind = "python"
	Rust    Kind = "rust"
	CMake   Kind = "cmake"
	DotNet  Kind = "dotnet"
	Node    Kind = "node"
	Static  Kind = "static"
	Generic Kind = "generic"
)

// Info describes a kind for the UI and for the model.
type Info struct {
	Kind  Kind
	Title string // shown to the user
	// Contract is what the model must respect for the workflow to build
	// the project. It is part of the prompt.
	Contract string
	// PerOS is false for kinds whose output does not depend on the OS.
	PerOS bool
}

var infos = []Info{
	{Kind: Auto, Title: "Авто — выберет модель", PerOS: true},
	{Kind: Go, Title: "Go — консольная программа", PerOS: true, Contract: "Stack: Go. Put go.mod in the repository root and `package main` in the root directory. " +
		"go.mod contains only the `module` and `go 1.22` lines: no `require` block and no go.sum, CI runs `go mod tidy` to resolve imports. " +
		"Prefer the standard library. CI runs `go test ./...` and `go build ./...`."},
	{Kind: GoFyne, Title: "Go + Fyne — оконное приложение", PerOS: true, Contract: "Stack: Go with the Fyne v2 GUI toolkit (import path fyne.io/fyne/v2). " +
		"Put go.mod in the repository root and `package main` in the root directory. " +
		"go.mod contains only the `module` and `go 1.22` lines: no `require` block and no go.sum, CI runs `go mod tidy` and gets the latest Fyne v2. " +
		"Update widgets from goroutines only inside fyne.Do(func(){...}). Embed resources with go:embed; no external asset files at runtime."},
	{Kind: Python, Title: "Python — исполняемый файл", PerOS: true, Contract: "Stack: Python 3.12. The entry point is main.py in the repository root. " +
		"List third-party packages in requirements.txt (omit the file if there are none). " +
		"CI packs main.py into a single executable with PyInstaller, so do not read data files relative to the source tree."},
	{Kind: Rust, Title: "Rust", PerOS: true, Contract: "Stack: Rust, edition 2021. Cargo.toml in the repository root, the program in src/main.rs. " +
		"No Cargo.lock. CI runs `cargo test --release` and `cargo build --release`."},
	{Kind: CMake, Title: "C/C++ — CMake", PerOS: true, Contract: "Stack: C++17 with CMake. CMakeLists.txt in the repository root (cmake_minimum_required 3.16). " +
		"It must contain `install(TARGETS <target> RUNTIME DESTINATION .)` — CI collects the program with `cmake --install`. " +
		"No third-party libraries. The code must compile with MSVC, GCC and Clang."},
	{Kind: DotNet, Title: ".NET — C#", PerOS: true, Contract: "Stack: C# on .NET 8 (TargetFramework net8.0). Exactly one .csproj in the repository root, no .sln. " +
		"CI runs `dotnet publish` as a self-contained single file."},
	{Kind: Node, Title: "Веб-приложение — Node.js", Contract: "Stack: a web app built with Node.js. package.json in the repository root with a `build` script " +
		"that writes the finished site into dist/ (Vite is a good default). No lock file: CI runs `npm install` and `npm run build`."},
	{Kind: Static, Title: "Статический сайт", Contract: "Stack: a static site. index.html in the repository root, plain HTML, CSS and JavaScript, no build step and no CDN dependencies."},
	{Kind: Generic, Title: "Другое — архив исходников", Contract: "There is no build recipe for this stack: CI only runs `make` when a Makefile exists and packs the sources."},
}

// Kinds returns every kind the user can pick, in display order.
func Kinds() []Info {
	out := make([]Info, len(infos))
	copy(out, infos)
	return out
}

// Lookup returns the description of a kind.
func Lookup(k Kind) (Info, bool) {
	for _, i := range infos {
		if i.Kind == k {
			return i, true
		}
	}
	return Info{}, false
}

// AutoContract is the prompt text used when the model picks the stack itself.
func AutoContract() string {
	var b strings.Builder
	b.WriteString("Choose the stack that fits the task best from the list below and follow its rules exactly — CI recognises the stack by these files:\n")
	for _, i := range infos {
		if i.Kind == Auto || i.Kind == Generic {
			continue
		}
		b.WriteString("- ")
		b.WriteString(i.Contract)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Detect decides the project kind from the files in the repository root.
// read returns a file's content ("" when unknown); it is only consulted for
// Go sources, to tell a Fyne GUI from a console program.
func Detect(paths []string, read func(path string) string) Kind {
	root := map[string]bool{}
	var goFiles []string
	rootPy, rootProj := false, false
	for _, p := range paths {
		if strings.HasSuffix(p, ".go") || p == "go.mod" {
			goFiles = append(goFiles, p)
		}
		if strings.Contains(p, "/") {
			continue
		}
		root[strings.ToLower(p)] = true
		switch strings.ToLower(path.Ext(p)) {
		case ".py":
			rootPy = true
		case ".csproj", ".fsproj", ".sln":
			rootProj = true
		}
	}
	switch {
	case root["go.mod"]:
		if read != nil {
			for _, p := range goFiles {
				if strings.Contains(read(p), "fyne.io/fyne") {
					return GoFyne
				}
			}
		}
		return Go
	case root["cargo.toml"]:
		return Rust
	case rootProj:
		return DotNet
	case root["cmakelists.txt"]:
		return CMake
	case root["package.json"]:
		return Node
	case root["main.py"] || root["pyproject.toml"] || root["requirements.txt"] || rootPy:
		return Python
	case root["index.html"]:
		return Static
	}
	return Generic
}

// Options configure a rendered workflow.
type Options struct {
	Kind Kind
	// Branch is the branch whose pushes start a build.
	Branch string
	// AppName names the artifacts; it is sanitised.
	AppName string
	// Targets are the operating systems to build for: windows, linux, macos.
	Targets []string
	// Release publishes a GitHub release with the builds after a good run.
	Release bool
}

type matrixRow struct {
	Target, Runner string
}

var runners = map[string]string{
	"windows": "windows-latest",
	"linux":   "ubuntu-latest",
	"macos":   "macos-latest",
}

// TargetNames lists the supported build targets in display order.
func TargetNames() []string { return []string{"windows", "linux", "macos"} }

//go:embed templates/*.yml
var templateFS embed.FS

var templates = template.Must(loadTemplates())

func loadTemplates() (*template.Template, error) {
	root := template.New("root").Delims("<%", "%>")
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(e.Name(), ".yml")
		text := string(data)
		if part, ok := strings.CutPrefix(name, "part-"); ok {
			// Parts are spliced into the middle of a line structure.
			name, text = part, strings.TrimRight(text, "\n")
		}
		if _, err := root.New(name).Parse(text); err != nil {
			return nil, fmt.Errorf("template %s: %w", e.Name(), err)
		}
	}
	return root, nil
}

// Render produces the workflow file for a project.
func Render(opts Options) (string, error) {
	info, ok := Lookup(opts.Kind)
	if !ok || opts.Kind == Auto {
		return "", fmt.Errorf("нет шаблона сборки для типа проекта %q", opts.Kind)
	}
	branch := strings.TrimSpace(opts.Branch)
	if branch == "" || strings.ContainsAny(branch, "\"\n\\") {
		return "", fmt.Errorf("недопустимое имя ветки %q", opts.Branch)
	}

	var matrix []matrixRow
	if info.PerOS {
		seen := map[string]bool{}
		for _, t := range opts.Targets {
			t = strings.ToLower(strings.TrimSpace(t))
			if runner, ok := runners[t]; ok && !seen[t] {
				seen[t] = true
				matrix = append(matrix, matrixRow{Target: t, Runner: runner})
			}
		}
		if len(matrix) == 0 {
			return "", fmt.Errorf("выберите хотя бы одну систему, для которой собирать приложение")
		}
		order := map[string]int{"windows": 0, "linux": 1, "macos": 2}
		sort.Slice(matrix, func(i, j int) bool { return order[matrix[i].Target] < order[matrix[j].Target] })
	}

	data := struct {
		Branch  string
		AppName string
		Matrix  []matrixRow
		Release bool
	}{branch, SafeName(opts.AppName), matrix, opts.Release}

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, string(opts.Kind), data); err != nil {
		return "", err
	}
	out := strings.TrimRight(buf.String(), "\n") + "\n"
	return out, nil
}

// SafeName reduces a repository name to characters that are safe in
// artifact names, file names and shell arguments.
func SafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "app"
	}
	return name
}
