// Package runlog keeps the application's own log on disk: every line of
// the journal of every run, so that what happened can be read after the
// window is closed or the next run has cleared the screen.
package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxSize is when the file is set aside as "<name>.1" and started anew.
const maxSize = 4 << 20

// Log appends lines to a file. The zero value and a nil *Log write nothing.
type Log struct {
	path string
	mu   sync.Mutex
	err  error
}

// New returns a log writing to path; an empty path gives a log that
// writes nothing.
func New(path string) *Log {
	if path == "" {
		return nil
	}
	return &Log{path: path}
}

// Path is the file the log writes to, "" when it writes nothing.
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Err is the first write error, nil while the log works.
func (l *Log) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Start marks the beginning of a run.
func (l *Log) Start(at time.Time, repo, branch, model, task string) {
	where := repo
	if branch != "" {
		where += " (" + branch + ")"
	}
	if model != "" {
		where += ", модель " + model
	}
	l.write("\n==== " + stamp(at) + "  " + where + "\n     Задача: " + flat(task, 2000) + "\n")
}

// Line writes one journal line. mark is the level sign shown on screen.
func (l *Log) Line(at time.Time, mark, text string) {
	l.write(stamp(at) + "  " + mark + " " + flat(text, 8000) + "\n")
}

func (l *Log) write(s string) {
	if l == nil || l.path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.append(s); err != nil && l.err == nil {
		l.err = err
	}
}

func (l *Log) append(s string) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(l.path); err == nil && info.Size()+int64(len(s)) > maxSize {
		// One generation of history is enough; a failed rename only
		// means the file keeps growing a little longer.
		_ = os.Remove(l.path + ".1")
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func stamp(at time.Time) string { return at.Format("2006-01-02 15:04:05") }

// flat keeps an entry on one line.
func flat(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}
