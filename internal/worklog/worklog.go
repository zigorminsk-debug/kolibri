// Package worklog keeps the project's work log: a Markdown file committed
// to the repository next to the code.
//
// A local model starts every request with an empty memory, and its context
// is too small for a large project to be written in one go. The log is what
// carries the work across requests: each entry says what was asked, what
// was done, which files changed, how the build ended and what is left. The
// pipeline shows the end of the log to the model with every request, so it
// continues instead of starting over.
package worklog

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Path is where the log lives in the repository.
const Path = ".kolibri/worklog.md"

// Dir is the directory the application owns in the repository; the model
// may not write there.
const Dir = ".kolibri/"

const header = "# Журнал работы\n\n" +
	"Этот файл ведёт Kolibri Forge. В нём записано, что было поручено модели, что она сделала " +
	"и что осталось: по нему работа продолжается после перерыва или сброса контекста модели. " +
	"Не редактируйте его вручную — программа дописывает записи в конец.\n"

const entryMark = "\n## "

// maxSize bounds the file: the oldest entries are dropped beyond it.
const maxSize = 200 << 10

// Entry is one step of the work.
type Entry struct {
	Time time.Time
	// Kind names the step: "Задача", "Исправление сборки", "Продолжение".
	Kind string
	// Task is what the user asked for.
	Task string
	// Reason is the build error that made the step necessary.
	Reason string
	// Model is the model that did the step.
	Model string
	// Summary is the model's one-line description of what it did.
	Summary string
	// Screenshots lists attached user screenshots examined during this step.
	Screenshots []string
	// Web lists brief descriptions of searches and pages read through
	// Lightpanda while carrying out this step.
	Web []string
	// Files lists the changes, like "+ main.go", "~ go.mod", "- old.txt".
	Files []string
	// Unfinished says why the step is incomplete, empty when it is whole.
	Unfinished string
	// Next is the model's note on what remains.
	Next string
}

// Append adds an entry to the end of the log and returns the new document.
func Append(doc string, e Entry) string {
	doc = strings.TrimRight(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	if doc == "" {
		doc = strings.TrimRight(header, "\n")
	}
	var b strings.Builder
	b.WriteString(doc)
	b.WriteString("\n")
	fmt.Fprintf(&b, "%s%s — %s\n\n", entryMark, e.Time.UTC().Format("2006-01-02 15:04 UTC"), oneLine(e.Kind, 80))
	if e.Task != "" {
		fmt.Fprintf(&b, "- **Задача:** %s\n", oneLine(e.Task, 700))
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, "- **Ошибка сборки:** %s\n", oneLine(e.Reason, 300))
	}
	if e.Model != "" {
		fmt.Fprintf(&b, "- **Модель:** %s\n", oneLine(e.Model, 80))
	}
	if e.Summary != "" {
		fmt.Fprintf(&b, "- **Сделано:** %s\n", oneLine(e.Summary, 300))
	}
	if len(e.Screenshots) > 0 {
		cleaned := make([]string, len(e.Screenshots))
		for i, s := range e.Screenshots {
			cleaned[i] = oneLine(s, 100)
		}
		fmt.Fprintf(&b, "- **Скриншоты:** %s\n", strings.Join(cleaned, ", "))
	}
	if len(e.Web) > 0 {
		const maxListed = 15
		items := e.Web
		more := ""
		if len(items) > maxListed {
			more = fmt.Sprintf(" и ещё %d", len(items)-maxListed)
			items = items[:maxListed]
		}
		cleaned := make([]string, len(items))
		for i, w := range items {
			cleaned[i] = oneLine(w, 120)
		}
		fmt.Fprintf(&b, "- **Интернет (Lightpanda):** %s%s\n", strings.Join(cleaned, "; "), more)
	}
	if len(e.Files) > 0 {
		const maxListed = 40
		files := e.Files
		more := ""
		if len(files) > maxListed {
			more = fmt.Sprintf(" и ещё %d", len(files)-maxListed)
			files = files[:maxListed]
		}
		quoted := make([]string, len(files))
		for i, f := range files {
			quoted[i] = "`" + strings.ReplaceAll(f, "`", "'") + "`"
		}
		fmt.Fprintf(&b, "- **Файлы:** %s%s\n", strings.Join(quoted, ", "), more)
	}
	if e.Unfinished != "" {
		fmt.Fprintf(&b, "- **Не завершено:** %s\n", oneLine(e.Unfinished, 300))
	}
	if next := strings.TrimSpace(e.Next); next != "" {
		b.WriteString("- **Осталось сделать:**\n")
		for _, line := range strings.Split(next, "\n") {
			line = strings.TrimRight(line, " \t\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			// Headings inside a note would be taken for new entries.
			line = strings.TrimLeft(line, "#")
			b.WriteString("  " + line + "\n")
		}
	}
	return trim(b.String())
}

// Note adds a result line ("Итог") to the last entry — how the build
// ended, or why the run stopped.
func Note(doc string, text string) string {
	doc = strings.TrimRight(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	if doc == "" || !strings.Contains(doc, entryMark) {
		return doc + "\n"
	}
	return trim(doc + "\n- **Итог:** " + oneLine(text, 600) + "\n")
}

// Tail returns the latest entries that fit into max characters, oldest
// first, for showing to the model. The newest entry is always included,
// cut if it alone is too long.
func Tail(doc string, max int) string {
	entries := split(doc)
	if len(entries) == 0 || max <= 0 {
		return ""
	}
	start := len(entries) - 1
	size := utf8.RuneCountInString(entries[start])
	for start > 0 {
		n := utf8.RuneCountInString(entries[start-1])
		if size+n > max {
			break
		}
		size += n
		start--
	}
	out := strings.TrimSpace(strings.Join(entries[start:], ""))
	if r := []rune(out); len(r) > max {
		out = string(r[:max]) + "…"
	}
	if start > 0 {
		out = fmt.Sprintf("(%d earlier entries omitted)\n\n", start) + out
	}
	return out
}

// Entries reports how many entries the log holds.
func Entries(doc string) int { return len(split(doc)) }

// LastNext returns the "what remains" note of the newest entry, or "".
func LastNext(doc string) string {
	entries := split(doc)
	if len(entries) == 0 {
		return ""
	}
	last := entries[len(entries)-1]
	const mark = "- **Осталось сделать:**\n"
	i := strings.Index(last, mark)
	if i < 0 {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(last[i+len(mark):], "\n") {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "  "))
	}
	return strings.Join(lines, "\n")
}

// split cuts the document into entries, each starting with its heading.
func split(doc string) []string {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	var entries []string
	for {
		i := strings.Index(doc, entryMark)
		if i < 0 {
			break
		}
		doc = doc[i+1:]
		end := strings.Index(doc, entryMark)
		if end < 0 {
			entries = append(entries, strings.TrimRight(doc, "\n")+"\n\n")
			break
		}
		entries = append(entries, strings.TrimRight(doc[:end], "\n")+"\n\n")
		doc = doc[end:]
	}
	return entries
}

// trim drops the oldest entries of a log that outgrew maxSize.
func trim(doc string) string {
	if len(doc) <= maxSize {
		return doc
	}
	entries := split(doc)
	size := 0
	start := len(entries)
	for start > 1 && size+len(entries[start-1]) <= maxSize/2 {
		start--
		size += len(entries[start])
	}
	if start == len(entries) {
		start = len(entries) - 1
	}
	return header + fmt.Sprintf("\n_Более ранние записи (%d) удалены, чтобы файл не рос без конца._\n\n", start) +
		strings.TrimRight(strings.Join(entries[start:], ""), "\n") + "\n"
}

// oneLine flattens text to a single line of at most max characters.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
