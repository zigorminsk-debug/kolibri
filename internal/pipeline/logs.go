package pipeline

import (
	"regexp"
	"strings"
)

var (
	logTimestamp = regexp.MustCompile(`^\x{FEFF}?\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z ?`)
	ansiEscape   = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
)

// ExtractFailure reduces a GitHub Actions job log to the part a model needs
// to fix a build: the command of the failing step and its output up to the
// error, at most max characters.
func ExtractFailure(log string, max int) string {
	raw := strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		l = logTimestamp.ReplaceAllString(l, "")
		l = ansiEscape.ReplaceAllString(l, "")
		lines = append(lines, strings.TrimRight(l, " \t\r"))
	}

	// The first error annotation marks the end of the failing step.
	errAt := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "##[error]") {
			errAt = i
			break
		}
	}
	start, end := 0, len(lines)
	if errAt >= 0 {
		end = errAt + 1
		for i := errAt; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "##[group]Run ") {
				start = i
				break
			}
		}
	} else if len(lines) > 80 {
		start = len(lines) - 80
	}

	var out []string
	inGroup := false
	for _, l := range lines[start:end] {
		switch {
		case strings.HasPrefix(l, "##[group]Run "):
			// Keep the command, drop the echoed script and environment.
			out = append(out, "$ "+strings.TrimPrefix(l, "##[group]Run "))
			inGroup = true
		case strings.HasPrefix(l, "##[group]"):
			inGroup = true
		case strings.HasPrefix(l, "##[endgroup]"):
			inGroup = false
		case inGroup:
		case strings.HasPrefix(l, "##[error]"):
			out = append(out, "ERROR: "+strings.TrimPrefix(l, "##[error]"))
		case strings.HasPrefix(l, "##[warning]"), strings.HasPrefix(l, "##[debug]"), strings.HasPrefix(l, "##[command]"):
		case strings.TrimSpace(l) == "":
		default:
			out = append(out, l)
		}
	}
	return squeeze(strings.Join(out, "\n"), max)
}

// squeeze keeps the head and the tail of a long text: compilers put the
// first errors at the top, test runners put the summary at the bottom.
func squeeze(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head, tail := max/3, max-max/3
	h := s[:head]
	if i := strings.LastIndex(h, "\n"); i > 0 {
		h = h[:i]
	}
	t := s[len(s)-tail:]
	if i := strings.Index(t, "\n"); i >= 0 && i < len(t)-1 {
		t = t[i+1:]
	}
	return strings.ToValidUTF8(h+"\n[... log shortened ...]\n"+t, "")
}
