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

func TestParseWebBlockFormats(t *testing.T) {
	// 1. Canonical JSONL with === NOTES ===, returning ErrNoFiles while populating Web and Notes.
	reply := "<think>should avoid === SEARCH: secret === here</think>\n" +
		"=== NOTES ===\nИзучаю устройство Radmin VPN и протокол туннеля.\n=== END NOTES ===\n\n" +
		"=== WEB ===\n" +
		`{"action":"search","query":"radmin vpn architecture","limit":5}` + "\n" +
		`{"action":"open","url":"https://www.radmin-vpn.com/"}` + "\n" +
		`{"action":"extract","selector":".feature","schema":{"title":"string"}}` + "\n" +
		`{"action":"click","node_id":7}` + "\n" +
		`{"action":"fill","selector":"input[name=q]","value":"mesh","submit":true}` + "\n" +
		"=== END WEB ===\n"
	res, err := Parse(reply, false)
	if !errors.Is(err, ErrNoFiles) {
		t.Fatalf("err = %v, want ErrNoFiles", err)
	}
	if res.Notes != "Изучаю устройство Radmin VPN и протокол туннеля." {
		t.Errorf("Notes = %q", res.Notes)
	}
	if len(res.Web) != 5 {
		t.Fatalf("len(Web) = %d: %+v", len(res.Web), res.Web)
	}
	if res.Web[0].Kind != "search" || res.Web[0].Query != "radmin vpn architecture" || res.Web[0].Limit != 5 {
		t.Errorf("Web[0] = %+v", res.Web[0])
	}
	if res.Web[1].Kind != "open" || res.Web[1].URL != "https://www.radmin-vpn.com/" {
		t.Errorf("Web[1] = %+v", res.Web[1])
	}
	if res.Web[2].Kind != "extract" || res.Web[2].Schema != `{"title":"string"}` {
		t.Errorf("Web[2] = %+v", res.Web[2])
	}
	if res.Web[3].Kind != "click" || res.Web[3].NodeID != 7 {
		t.Errorf("Web[3] = %+v", res.Web[3])
	}
	if res.Web[4].Kind != "fill" || res.Web[4].Value != "mesh" || !res.Web[4].Submit {
		t.Errorf("Web[4] = %+v", res.Web[4])
	}

	// 2. Fenced multi-line JSON array, field aliases, MCP-style arguments, and direction helper.
	drifted := "=== WEB ===\n```json\n[\n" +
		`  {"tool":"goto","arguments":{"href":"https://wireguard.com/protocol/"}},` + "\n" +
		`  {"type":"scroll","direction":"down"},` + "\n" +
		`  {"q":"golang tun tap windows wintun"}` + "\n" +
		"]\n```\n=== END WEB ===\n"
	res, err = Parse(drifted, false)
	if !errors.Is(err, ErrNoFiles) || len(res.Web) != 3 {
		t.Fatalf("drifted: err=%v web=%+v", err, res.Web)
	}
	if res.Web[0].Kind != "open" || res.Web[0].URL != "https://wireguard.com/protocol/" {
		t.Errorf("drifted[0] = %+v", res.Web[0])
	}
	if res.Web[1].Kind != "scroll" || res.Web[1].Y <= 0 {
		t.Errorf("drifted[1] = %+v", res.Web[1])
	}
	if res.Web[2].Kind != "search" || res.Web[2].Query != "golang tun tap windows wintun" {
		t.Errorf("drifted[2] = %+v", res.Web[2])
	}

	// 3. Plain-text shorthand and single-line markers.
	short := "=== SEARCH: radmin vpn nat traversal ===\n=== OPEN: https://example.com/spec ===\n" +
		"=== WEB ===\n- поиск: udp hole punching go\n- links\n- click: #12\n=== END WEB ===\n"
	res, _ = Parse(short, false)
	if len(res.Web) != 5 || res.Web[0].Query != "radmin vpn nat traversal" || res.Web[1].URL != "https://example.com/spec" ||
		res.Web[2].Query != "udp hole punching go" || res.Web[3].Kind != "links" || res.Web[4].NodeID != 12 {
		t.Fatalf("shorthand web = %+v", res.Web)
	}

	// 4. Mixed turn: model writes files AND requests another web lookup before finishing.
	mixed := "SUMMARY: Базовый каркас VPN\n\n=== FILE: go.mod ===\nmodule localvpn\n\ngo 1.24\n=== END FILE ===\n\n" +
		"=== WEB ===\n" + `{"action":"search","query":"water tun library windows api"}` + "\n=== END WEB ===\n"
	res, err = Parse(mixed, false)
	if err != nil {
		t.Fatalf("mixed err = %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "go.mod" || len(res.Web) != 1 || res.Web[0].Kind != "search" {
		t.Fatalf("mixed = %+v", res)
	}

	// 5. Cut-off inside a second JSON line preserves the first complete action.
	cut := "=== WEB ===\n" + `{"action":"search","query":"first"}` + "\n" + `{"action":"open","url":"https://exa`
	res, _ = Parse(cut, true)
	if len(res.Web) != 1 || res.Web[0].Query != "first" {
		t.Fatalf("cut web = %+v", res.Web)
	}

	// 6. === WEB === and === END WEB === inside a file block stay file content.
	doc := "=== WEB ===\n{\"action\":\"search\",\"query\":\"demo\"}\n=== END WEB ===\n"
	res, err = Parse("=== FILE: docs/protocol.md ===\n"+doc+"=== END FILE ===\n", false)
	if err != nil || len(res.Web) != 0 || res.Files[0].Content != doc {
		t.Fatalf("web inside file: err=%v web=%+v files=%+v", err, res.Web, res.Files)
	}
}
