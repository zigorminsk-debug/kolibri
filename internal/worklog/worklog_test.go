package worklog

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)

func TestAppendAndNote(t *testing.T) {
	doc := Append("", Entry{
		Time: at, Kind: "Задача", Task: "Оконный калькулятор\nс историей", Model: "qwen3",
		Summary: "Каркас приложения", Files: []string{"+ main.go", "~ go.mod"},
		Next: "- экран настроек\n\n## не заголовок\n- сохранение",
	})
	for _, want := range []string{
		"# Журнал работы\n",
		"\n## 2026-11-15 08:30 UTC — Задача\n",
		"- **Задача:** Оконный калькулятор с историей\n",
		"- **Модель:** qwen3\n",
		"- **Сделано:** Каркас приложения\n",
		"- **Файлы:** `+ main.go`, `~ go.mod`\n",
		"- **Осталось сделать:**\n  - экран настроек\n   не заголовок\n  - сохранение\n",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("log lacks %q:\n%s", want, doc)
		}
	}
	if Entries(doc) != 1 {
		t.Errorf("Entries = %d", Entries(doc))
	}
	if got := LastNext(doc); got != "- экран настроек\n не заголовок\n- сохранение" {
		t.Errorf("LastNext = %q", got)
	}

	doc = Note(doc, "сборка №7 прошла")
	if !strings.HasSuffix(doc, "- **Итог:** сборка №7 прошла\n") {
		t.Errorf("note not at the end:\n%s", doc)
	}

	doc = Append(doc, Entry{Time: at.Add(time.Hour), Kind: "Продолжение", Summary: "Экран настроек", Unfinished: "ответ оборвался на файле ui.go"})
	if Entries(doc) != 2 || !strings.Contains(doc, "- **Не завершено:** ответ оборвался на файле ui.go\n") {
		t.Errorf("second entry:\n%s", doc)
	}
	if LastNext(doc) != "" {
		t.Errorf("LastNext of an entry without a note = %q", LastNext(doc))
	}
	// A note on an empty log has nothing to attach to.
	if Entries(Note("", "x")) != 0 {
		t.Error("Note must not invent an entry")
	}
}

func TestTailKeepsNewestEntries(t *testing.T) {
	doc := ""
	for i := 1; i <= 10; i++ {
		doc = Append(doc, Entry{Time: at.Add(time.Duration(i) * time.Hour), Kind: "Задача", Summary: fmt.Sprintf("шаг %d %s", i, strings.Repeat("я", 100))})
	}
	tail := Tail(doc, 500)
	if !strings.Contains(tail, "шаг 10 ") || strings.Contains(tail, "шаг 1 ") {
		t.Errorf("tail must hold the newest entries:\n%s", tail)
	}
	if !strings.HasPrefix(tail, "(") || !strings.Contains(tail, "earlier entries omitted") {
		t.Errorf("tail must say that entries were omitted:\n%s", tail)
	}
	if strings.Contains(tail, "# Журнал работы") {
		t.Error("tail must not repeat the file header")
	}
	if n := len([]rune(tail)); n > 560 {
		t.Errorf("tail is %d characters", n)
	}
	// One oversized entry is cut, not dropped.
	if got := Tail(doc, 50); !strings.Contains(got, "## 2026-11-15 18:30 UTC") {
		t.Errorf("tiny tail = %q", got)
	}
	if all := Tail(doc, 1<<20); strings.Contains(all, "omitted") || !strings.Contains(all, "шаг 1 ") {
		t.Errorf("full tail is wrong")
	}
	if Tail("", 100) != "" || Tail(doc, 0) != "" {
		t.Error("empty cases")
	}
}

func TestLogDoesNotGrowForever(t *testing.T) {
	doc := ""
	for i := 1; i <= 400; i++ {
		doc = Append(doc, Entry{Time: at, Kind: "Задача", Summary: fmt.Sprintf("шаг-%d", i), Next: strings.Repeat("- пункт плана\n", 60)})
	}
	if len(doc) > maxSize {
		t.Errorf("log is %d bytes, limit %d", len(doc), maxSize)
	}
	if !strings.Contains(doc, "шаг-400") || strings.Contains(doc, "шаг-1\n") {
		t.Error("the newest entries must survive, the oldest must go")
	}
	if !strings.HasPrefix(doc, "# Журнал работы") || !strings.Contains(doc, "удалены") {
		t.Error("a trimmed log keeps its header and says it was trimmed")
	}
}
