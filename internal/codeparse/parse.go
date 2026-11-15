// Package codeparse turns a model's reply into a set of file changes.
//
// The pipeline asks the model for a strict block format:
//
//	SUMMARY: one-line commit message
//
//	=== FILE: path/to/file.ext ===
//	...complete content...
//	=== END FILE ===
//
//	=== DELETE: path/to/old.ext ===
//
// Local models drift from formats, so the parser is forgiving: a missing END
// marker, Markdown fences around the content, and a plain Markdown reply
// ("### main.go" followed by a fenced block) are all understood.
package codeparse

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// File is one change: new content for Path, or its removal.
type File struct {
	Path    string
	Content string
	Delete  bool
}

// Result is a parsed reply.
type Result struct {
	// Summary is the model's one-line description, empty when it gave none.
	Summary string
	Files   []File
	// Truncated is set when the reply ended inside a file block, which is
	// what a reply cut off by the token limit looks like. The unfinished
	// file is not included in Files.
	Truncated bool
	// TruncatedPath is the file the reply was cut in.
	TruncatedPath string
	// Rejected lists paths the model sent that cannot be committed.
	Rejected []string
}

// ErrNoFiles means the reply held no file blocks at all.
var ErrNoFiles = errors.New("в ответе модели нет файлов в ожидаемом формате")

var (
	fileMarker   = regexp.MustCompile(`(?i)^\s*={2,}\s*FILE\s*:\s*(.+?)\s*=*\s*$`)
	deleteMarker = regexp.MustCompile(`(?i)^\s*={2,}\s*DELETE\s*:\s*(.+?)\s*=*\s*$`)
	endMarker    = regexp.MustCompile(`(?i)^\s*={2,}\s*END(\s+OF)?(\s+FILE)?\s*={2,}\s*$`)
	summaryLine  = regexp.MustCompile(`(?i)^\s*\**SUMMARY\**\s*:\**\s*(.+?)\s*$`)
	thinkBlock   = regexp.MustCompile(`(?s)<think>.*?</think>`)
	fenceLine    = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([^`]*)$")
)

// Parse reads a model reply. cutOff tells the parser the reply was stopped
// by the token limit, so a file block left open at the end is incomplete
// rather than merely missing its END marker.
func Parse(reply string, cutOff bool) (Result, error) {
	reply = strings.ReplaceAll(reply, "\r\n", "\n")
	reply = thinkBlock.ReplaceAllString(reply, "")
	// An unclosed <think> means everything after it is reasoning.
	if i := strings.Index(reply, "<think>"); i >= 0 && !strings.Contains(reply[i:], "</think>") {
		reply = reply[:i]
	}

	res := parseBlocks(reply, cutOff)
	if len(res.Files) == 0 && !res.Truncated {
		md := parseMarkdown(reply, cutOff)
		md.Rejected = append(res.Rejected, md.Rejected...)
		if md.Summary == "" {
			md.Summary = res.Summary
		}
		res = md
	}
	res.Files = dedupe(res.Files)
	if len(res.Files) == 0 {
		return res, ErrNoFiles
	}
	return res, nil
}

func parseBlocks(reply string, cutOff bool) Result {
	var res Result
	lines := strings.Split(reply, "\n")

	open := false
	var curPath string
	var buf []string
	closeFile := func() {
		if !open {
			return
		}
		open = false
		clean, ok := CleanPath(curPath)
		if !ok {
			res.Rejected = append(res.Rejected, curPath)
			return
		}
		res.Files = append(res.Files, File{Path: clean, Content: cleanContent(buf)})
	}

	for _, line := range lines {
		if m := fileMarker.FindStringSubmatch(line); m != nil {
			closeFile()
			open, curPath, buf = true, m[1], nil
			continue
		}
		if m := deleteMarker.FindStringSubmatch(line); m != nil {
			closeFile()
			if clean, ok := CleanPath(m[1]); ok {
				res.Files = append(res.Files, File{Path: clean, Delete: true})
			} else {
				res.Rejected = append(res.Rejected, m[1])
			}
			continue
		}
		if endMarker.MatchString(line) {
			closeFile()
			continue
		}
		if open {
			buf = append(buf, line)
			continue
		}
		if res.Summary == "" && len(res.Files) == 0 {
			if m := summaryLine.FindStringSubmatch(line); m != nil {
				res.Summary = cleanSummary(m[1])
			}
		}
	}
	if open {
		if cutOff {
			res.Truncated = true
			res.TruncatedPath, _ = CleanPath(curPath)
		} else {
			closeFile()
		}
	}
	return res
}

// pathToken matches something that looks like a relative file path.
var pathToken = regexp.MustCompile(`^[\w@.+\-]+(?:[/\\][\w@.+\-]+)*$`)

// knownBare lists file names without an extension that are still files.
var knownBare = map[string]bool{
	"makefile": true, "dockerfile": true, "license": true, "procfile": true,
	"gemfile": true, "rakefile": true, "cmakelists.txt": true, ".gitignore": true,
}

func looksLikePath(s string) bool {
	if s == "" || len(s) > 200 || !pathToken.MatchString(s) {
		return false
	}
	base := s
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		base = s[i+1:]
	}
	if knownBare[strings.ToLower(base)] {
		return true
	}
	dot := strings.LastIndex(base, ".")
	return dot >= 0 && dot < len(base)-1
}

var headingPrefix = regexp.MustCompile(`^(?:#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)*`)

var headingNoise = regexp.MustCompile(`(?i)^(?:file|файл|path|путь|filename)\s*[:：]\s*`)

// headingPath extracts a path from a line that introduces a code block,
// such as "### `cmd/main.go`", "**main.go**:" or "File: main.go".
func headingPath(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if s == "" || len(s) > 220 {
		return "", false
	}
	s = headingPrefix.ReplaceAllString(s, "")
	s = strings.Trim(s, "*_ \t")
	s = headingNoise.ReplaceAllString(s, "")
	s = strings.TrimSuffix(strings.TrimSpace(s), ":")
	s = strings.Trim(s, "*_`'\" \t")
	if looksLikePath(s) {
		return s, true
	}
	return "", false
}

var commentPath = regexp.MustCompile(`^\s*(?://|#|--|;|/\*|<!--)\s*(?i:file\s*:\s*)?([^\s*]+)\s*(?:\*/|-->)?\s*$`)

func parseMarkdown(reply string, cutOff bool) Result {
	var res Result
	lines := strings.Split(reply, "\n")
	for i := 0; i < len(lines); i++ {
		if res.Summary == "" && len(res.Files) == 0 {
			if m := summaryLine.FindStringSubmatch(lines[i]); m != nil {
				res.Summary = cleanSummary(m[1])
				continue
			}
		}
		m := fenceLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		fence, info := m[1], strings.TrimSpace(m[2])
		// Find the closing fence: same character, at least as long.
		end := -1
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
				end = j
				break
			}
		}
		body := lines[i+1:]
		closed := end >= 0
		if closed {
			body = lines[i+1 : end]
		}

		name, found := "", false
		// "```go:main.go" or "```go main.go" or "```main.go"
		for _, tok := range strings.FieldsFunc(info, func(r rune) bool { return r == ':' || r == ' ' || r == '=' || r == '"' }) {
			if looksLikePath(tok) && strings.ContainsAny(tok, "./") {
				name, found = tok, true
			}
		}
		// A heading within the two non-empty lines above the block.
		if !found {
			seen := 0
			for j := i - 1; j >= 0 && seen < 2; j-- {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				seen++
				if p, ok := headingPath(lines[j]); ok {
					name, found = p, true
					break
				}
			}
		}
		// A leading comment naming the file: "// cmd/main.go".
		if !found && len(body) > 0 {
			if cm := commentPath.FindStringSubmatch(body[0]); cm != nil && looksLikePath(cm[1]) && strings.ContainsAny(cm[1], "./") {
				name, found = cm[1], true
			}
		}

		if found {
			if !closed && cutOff {
				res.Truncated = true
				res.TruncatedPath, _ = CleanPath(name)
			} else if clean, ok := CleanPath(name); ok {
				res.Files = append(res.Files, File{Path: clean, Content: joinContent(body)})
			} else {
				res.Rejected = append(res.Rejected, name)
			}
		}
		if !closed {
			break
		}
		i = end
	}
	return res
}

// cleanContent strips a Markdown fence the model wrapped around a whole file
// despite being told not to, and normalises the trailing newline.
func cleanContent(lines []string) string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	lines = lines[start:end]
	if len(lines) >= 2 {
		first := fenceLine.FindStringSubmatch(lines[0])
		last := strings.TrimSpace(lines[len(lines)-1])
		if first != nil && strings.HasPrefix(last, first[1]) && strings.Trim(last, first[1][:1]) == "" {
			inner := lines[1 : len(lines)-1]
			// Only unwrap when the fence really encloses the whole file: an
			// inner fence line of the same kind would mean these are two
			// separate blocks of a Markdown document.
			wraps := true
			for _, l := range inner {
				t := strings.TrimSpace(l)
				if strings.HasPrefix(t, first[1]) && strings.Trim(t, first[1][:1]) == "" {
					wraps = false
					break
				}
			}
			if wraps {
				lines = inner
			}
		}
	}
	return joinContent(lines)
}

func joinContent(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func cleanSummary(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "*`\"' ")
	if utf8.RuneCountInString(s) > 100 {
		r := []rune(s)
		s = string(r[:99]) + "…"
	}
	return s
}

// CleanPath normalises a path from a model reply and reports whether it is
// safe to commit: relative, inside the repository, and not Git's own data.
func CleanPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "`'\"*")
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" || len(p) > 300 || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return "", false
	}
	if len(p) >= 2 && p[1] == ':' { // C:\...
		return "", false
	}
	if strings.ContainsAny(p, "\x00\n\r\t<>|?*\"") || strings.HasSuffix(p, "/") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	p = path.Clean(p)
	if p == "." || p == ".git" || strings.HasPrefix(p, ".git/") {
		return "", false
	}
	return p, true
}

// dedupe keeps the last change for every path, preserving first-seen order.
func dedupe(files []File) []File {
	index := make(map[string]int, len(files))
	out := files[:0:0]
	for _, f := range files {
		if i, ok := index[f.Path]; ok {
			out[i] = f
			continue
		}
		index[f.Path] = len(out)
		out = append(out, f)
	}
	return out
}
