package codeparse

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseBlocks(t *testing.T) {
	reply := "Sure!\nSUMMARY: Add greeter\n\n=== FILE: go.mod ===\nmodule demo\n\ngo 1.22\n=== END FILE ===\n\n" +
		"=== FILE: ./cmd\\app\\main.go ===\n```go\npackage main\n\nfunc main() {}\n```\n=== END FILE ===\n" +
		"=== DELETE: old.txt ===\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []File{
		{Path: "go.mod", Content: "module demo\n\ngo 1.22\n"},
		{Path: "cmd/app/main.go", Content: "package main\n\nfunc main() {}\n"},
		{Path: "old.txt", Delete: true},
	}
	if !reflect.DeepEqual(res.Files, want) {
		t.Fatalf("files:\n got %#v\nwant %#v", res.Files, want)
	}
	if res.Summary != "Add greeter" {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.Truncated {
		t.Error("unexpected Truncated")
	}
}

func TestParseMissingEndMarkers(t *testing.T) {
	reply := "=== FILE: a.txt ===\none\n=== FILE: b.txt ===\ntwo\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []File{{Path: "a.txt", Content: "one\n"}, {Path: "b.txt", Content: "two\n"}}
	if !reflect.DeepEqual(res.Files, want) {
		t.Fatalf("got %#v", res.Files)
	}
}

func TestParseCutOffDropsUnfinishedFile(t *testing.T) {
	reply := "=== FILE: a.txt ===\none\n=== END FILE ===\n=== FILE: big.go ===\npackage main\nfunc ma"
	res, err := Parse(reply, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "a.txt" {
		t.Fatalf("got %#v", res.Files)
	}
	if !res.Truncated || res.TruncatedPath != "big.go" {
		t.Errorf("Truncated=%v path=%q", res.Truncated, res.TruncatedPath)
	}
}

func TestParseKeepsInnerFencesOfMarkdownFiles(t *testing.T) {
	readme := "# Demo\n\n```sh\ngo run .\n```\n\nand\n\n```sh\ngo test\n```\n"
	res, err := Parse("=== FILE: README.md ===\n"+readme+"=== END FILE ===\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Content != readme {
		t.Fatalf("content changed:\n%q", res.Files[0].Content)
	}
}

func TestParseLastChangeWins(t *testing.T) {
	res, err := Parse("=== FILE: a ===\n1\n=== END ===\n=== FILE: a.txt ===\nx\n=== END ===\n=== FILE: a.txt ===\ny\n=== END ===\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Files[1].Content != "y\n" {
		t.Fatalf("got %#v", res.Files)
	}
}

func TestParseStripsThinking(t *testing.T) {
	reply := "<think>I could write === FILE: evil.go === here</think>\n=== FILE: ok.go ===\npackage ok\n=== END FILE ===\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "ok.go" {
		t.Fatalf("got %#v", res.Files)
	}
}

func TestParseMarkdownFallback(t *testing.T) {
	reply := "Вот проект.\n\n### `main.py`\n\n```python\nprint('hi')\n```\n\n**requirements.txt**:\n```\nrequests\n```\n\n" +
		"```go:cmd/tool/main.go\npackage main\n```\n\n```js\n// src/index.js\nconsole.log(1)\n```\n\nЗапуск:\n```sh\npython main.py\n```\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range res.Files {
		got[f.Path] = f.Content
	}
	want := map[string]string{
		"main.py":          "print('hi')\n",
		"requirements.txt": "requests\n",
		"cmd/tool/main.go": "package main\n",
		"src/index.js":     "// src/index.js\nconsole.log(1)\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestParseMarkdownDotfileAndNumberedHeading(t *testing.T) {
	reply := "1. `.gitignore`\n```\ndist/\n```\n\n2. **2048.go**\n```go\npackage main\n```\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 2 || res.Files[0].Path != ".gitignore" || res.Files[1].Path != "2048.go" {
		t.Fatalf("got %#v", res.Files)
	}
}

func TestParseNoFiles(t *testing.T) {
	_, err := Parse("Я не могу это сделать.\n```\nls -la\n```", false)
	if !errors.Is(err, ErrNoFiles) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRejectsUnsafePaths(t *testing.T) {
	reply := "=== FILE: ../secret ===\nx\n=== END FILE ===\n=== FILE: /etc/passwd ===\nx\n=== END FILE ===\n" +
		"=== FILE: .git/config ===\nx\n=== END FILE ===\n=== FILE: C:\\Windows\\a.txt ===\nx\n=== END FILE ===\n=== FILE: fine.txt ===\nx\n=== END FILE ===\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "fine.txt" {
		t.Fatalf("got %#v", res.Files)
	}
	if len(res.Rejected) != 4 {
		t.Errorf("rejected = %v", res.Rejected)
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"`src/main.go`":   "src/main.go",
		"./a//b.txt":      "a/b.txt",
		"a/./b.txt":       "a/b.txt",
		".github/x.yml":   ".github/x.yml",
		".gitignore":      ".gitignore",
		"dir/":            "",
		"a/../../b":       "",
		"":                "",
		".git":            "",
		"~/.ssh/id_rsa":   "",
		"name with sp.go": "name with sp.go",
	}
	for in, want := range cases {
		got, ok := CleanPath(in)
		if (want == "") == ok || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestParseNextBlock(t *testing.T) {
	reply := "SUMMARY: Каркас приложения\n\n=== FILE: main.go ===\npackage main\n=== END FILE ===\n\n" +
		"=== NEXT ===\n- экран настроек\n- сохранение в файл\n=== END NEXT ===\n"
	res, err := Parse(reply, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Content != "package main\n" {
		t.Errorf("files = %+v", res.Files)
	}
	if res.Next != "- экран настроек\n- сохранение в файл" {
		t.Errorf("Next = %q", res.Next)
	}

	// A note that says there is nothing left is not a plan.
	for _, body := range []string{"", "none", "- Nothing.", "нет", "-"} {
		res, err := Parse("=== FILE: a.txt ===\nx\n=== END FILE ===\n=== NEXT ===\n"+body+"\n=== END NEXT ===\n", false)
		if err != nil || res.Next != "" {
			t.Errorf("body %q: Next = %q, err = %v", body, res.Next, err)
		}
	}

	// The note may be cut off, or a file may follow it.
	res, _ = Parse("=== FILE: a.txt ===\nx\n=== END FILE ===\n=== NEXT ===\n- тесты", true)
	if res.Next != "- тесты" || res.Truncated {
		t.Errorf("cut note: %+v", res)
	}
	res, _ = Parse("=== NEXT ===\n- тесты\n=== FILE: a.txt ===\nx\n=== END FILE ===\n", false)
	if res.Next != "- тесты" || len(res.Files) != 1 {
		t.Errorf("note before file: %+v", res)
	}

	// Inside a file the marker is just content.
	res, _ = Parse("=== FILE: doc.md ===\n=== NEXT ===\ntext\n=== END FILE ===\n", false)
	if res.Next != "" || res.Files[0].Content != "=== NEXT ===\ntext\n" {
		t.Errorf("marker inside a file: %+v", res)
	}
}
