package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 15, 9, 5, 7, 0, time.UTC)

func TestLogWritesRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "kolibri-forge.log")
	l := New(path)
	l.Start(at, "me/demo", "main", "qwen3", "Сделай\nкалькулятор")
	l.Line(at.Add(time.Second), "·", "Отправляю запрос")
	l.Line(at.Add(2*time.Second), "×", "Сборка\nне прошла")
	if err := l.Err(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n==== 2026-11-15 09:05:07  me/demo (main), модель qwen3\n     Задача: Сделай калькулятор\n" +
		"2026-11-15 09:05:08  · Отправляю запрос\n2026-11-15 09:05:09  × Сборка не прошла\n"
	if string(data) != want {
		t.Errorf("log =\n%q\nwant\n%q", data, want)
	}
	if info, _ := os.Stat(path); info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("log is readable by others: %v", info.Mode())
	}
}

func TestLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kolibri-forge.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxSize-10)), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(path)
	l.Line(at, "·", "новая строка после переполнения")
	fresh, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if !strings.Contains(string(fresh), "новая строка") || len(fresh) > 200 || len(old) != maxSize-10 {
		t.Errorf("fresh = %d bytes, old = %d bytes", len(fresh), len(old))
	}
}

func TestDisabledLog(t *testing.T) {
	var l *Log
	l.Start(at, "a/b", "", "", "x")
	l.Line(at, "·", "x")
	if l.Path() != "" || l.Err() != nil || New("") != nil {
		t.Error("a nil log must be a quiet no-op")
	}
	// An unwritable place is reported once, not panicked on.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	bad := New(filepath.Join(blocker, "sub", "log.txt"))
	bad.Line(at, "·", "x")
	if bad.Err() == nil {
		t.Error("want an error for an unwritable path")
	}
}
