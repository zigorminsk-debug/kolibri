package ui

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/config"
	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/lightpanda"
	"github.com/zigorminsk-debug/kolibri/internal/pipeline"
	"github.com/zigorminsk-debug/kolibri/internal/runlog"
)

var shots = flag.String("shots", "", "write screenshots of the main states into this directory")

func newTestUI(t *testing.T, cfg config.Config, w, h float32) (*UI, fyne.Window) {
	t.Helper()
	app := test.NewTempApp(t)
	app.Settings().SetTheme(Theme{})
	win := app.NewWindow(AppName)
	t.Cleanup(win.Close)
	u := New(app, win, cfg)
	u.Save = func(c config.Config) error { return nil }
	var uiMu sync.Mutex
	u.runUI = func(fn func()) {
		uiMu.Lock()
		defer uiMu.Unlock()
		fn()
	}
	win.Resize(fyne.NewSize(w, h))
	return u, win
}

func snap(t *testing.T, win fyne.Window, name string) {
	t.Helper()
	if *shots == "" {
		return
	}
	if err := os.MkdirAll(*shots, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(*shots, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, win.Canvas().Capture()); err != nil {
		t.Fatal(err)
	}
}

func demoConfig() config.Config {
	cfg := config.Default()
	cfg.ColibriURL = "http://127.0.0.1:1"
	cfg.GitHubToken = ""
	cfg.OutputDir = filepath.Join("C:", "Users", "igor", "KolibriForge")
	cfg.Repo = "zigorminsk-debug/calc"
	cfg.ProjectKind = "go-fyne"
	cfg.LastTask = "Оконный калькулятор: четыре действия, проценты, история последних десяти вычислений. Тёмная тема, крупные кнопки, работа с клавиатуры."
	return cfg
}

const demoReply = "SUMMARY: Калькулятор с историей\n\n=== FILE: go.mod ===\nmodule calc\n\ngo 1.22\n=== END FILE ===\n\n" +
	"=== FILE: main.go ===\npackage main\n\nimport (\n\t\"fyne.io/fyne/v2/app\"\n\t\"fyne.io/fyne/v2/container\"\n\t\"fyne.io/fyne/v2/widget\"\n)\n\n" +
	"func main() {\n\ta := app.New()\n\tw := a.NewWindow(\"Калькулятор\")\n\tdisplay := widget.NewLabel(\"0\")\n\thistory := widget.NewList(\n"

func demoFiles() []pipeline.FileChange {
	return []pipeline.FileChange{
		{Path: ".github/workflows/kolibri-build.yml", Status: "new", Content: "name: Kolibri Build\non:\n  push:\n    branches: [\"main\"]\n"},
		{Path: "calc/engine.go", Status: "new", Content: "package calc\n\n// Eval computes a op b.\nfunc Eval(a, b float64, op rune) (float64, error) {\n\tswitch op {\n\tcase '+':\n\t\treturn a + b, nil\n\tcase '-':\n\t\treturn a - b, nil\n\t}\n\treturn 0, errUnknown\n}\n"},
		{Path: "go.mod", Status: "new", Content: "module calc\n\ngo 1.22\n"},
		{Path: "main.go", Status: "changed", Content: strings.Repeat("package main\n\nfunc main() {\n\trun()\n}\n\n", 12)},
		{Path: "old_notes.txt", Status: "deleted"},
	}
}

func demoRuns(done, ok bool) []pipeline.RunStatus {
	start := time.Now().Add(-3*time.Minute - 12*time.Second)
	jobs := []ghapi.Job{
		{Name: "Build (windows)", Status: "in_progress", StartedAt: start, Steps: []ghapi.Step{{Name: "Set up job", Status: "completed", Conclusion: "success"}, {Name: "Build", Status: "in_progress"}}},
		{Name: "Build (linux)", Status: "completed", Conclusion: "success", StartedAt: start, CompletedAt: start.Add(2*time.Minute + 5*time.Second)},
		{Name: "Release", Status: "queued"},
	}
	run := ghapi.Run{ID: 1, Name: "Kolibri Build", RunNumber: 7, Status: "in_progress", HTMLURL: "https://github.com/zigorminsk-debug/calc/actions/runs/1"}
	if done {
		run.Status = "completed"
		run.Conclusion = "success"
		jobs[0].Status, jobs[0].Conclusion, jobs[0].CompletedAt = "completed", "success", start.Add(3*time.Minute)
		jobs[2].Status, jobs[2].Conclusion = "completed", "success"
		if !ok {
			run.Conclusion = "failure"
			jobs[0].Conclusion = "failure"
			jobs[0].Steps[1] = ghapi.Step{Name: "Build", Status: "completed", Conclusion: "failure"}
			jobs[2].Conclusion = "skipped"
		}
	}
	return []pipeline.RunStatus{{Run: run, Jobs: jobs}}
}

// play feeds the UI the events of a typical run up to the given stage.
func play(u *UI, upTo pipeline.Stage) {
	o := observer{u}
	u.running = true
	u.started = time.Now().Add(-4*time.Minute - 12*time.Second)
	u.setFormEnabled(false)
	o.Stage(pipeline.StagePrepare, 1, 4)
	o.Log(pipeline.Info, "Колибри на связи: модель qwen3-coder-30b-a3b, контекст 65536 токенов")
	o.Log(pipeline.Info, "GitHub: вошли как zigorminsk-debug")
	o.Log(pipeline.Info, "Репозитория zigorminsk-debug/calc нет — создаю (приватный)")
	if upTo < pipeline.StageGenerate {
		return
	}
	o.Stage(pipeline.StageGenerate, 1, 4)
	o.Log(pipeline.Info, "Отправляю запрос модели qwen3-coder-30b-a3b (2.4 КБ)")
	o.ModelStart("Задача")
	o.ModelDelta("Нужен калькулятор на Fyne. План: окно с дисплеем, сетка кнопок 4×5, список истории справа. Логику вычислений вынесу в отдельный пакет, чтобы её можно было проверить тестами без окна.\n", true)
	o.ModelDelta(demoReply, false)
	u.stream.flush()
	if upTo < pipeline.StageCommit {
		return
	}
	o.Log(pipeline.Info, "Ответ получен: 6.8 КБ, 1912 токенов за 5 мин 41 с (5.6 ток/с)")
	o.Log(pipeline.Info, "Тип проекта: Go + Fyne — оконное приложение")
	o.Files(demoFiles())
	o.Stage(pipeline.StageCommit, 1, 4)
	o.Log(pipeline.Good, "Коммит 3f9c2ab в zigorminsk-debug/calc (main): 4 файла — Калькулятор с историей")
	if upTo < pipeline.StageBuild {
		return
	}
	o.Stage(pipeline.StageBuild, 1, 4)
	o.Log(pipeline.Info, "Жду, когда GitHub Actions начнёт сборку…")
	o.Log(pipeline.Info, "Сборка №7 «Kolibri Build» запущена: https://github.com/zigorminsk-debug/calc/actions/runs/19284756102")
	o.Build(demoRuns(false, true))
}

func TestScreens(t *testing.T) {
	// 1. First start: nothing configured, nothing running.
	cfg := config.Default()
	cfg.ColibriURL = "http://127.0.0.1:1"
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	u, win := newTestUI(t, cfg, 1240, 800)
	u.llmChip.Set("Колибри · нет связи", toneBad)
	u.ghChip.Set("GitHub · нужен токен", toneBad)
	snap(t, win, "01-first-start")

	u.onRunTapped()
	if !u.problem.Visible() || !strings.Contains(u.problem.Text, "владелец/название") {
		t.Errorf("empty form: problem = %q (visible %v)", u.problem.Text, u.problem.Visible())
	}
	snap(t, win, "02-validation")

	// 2. A run in its stages.
	u, win = newTestUI(t, demoConfig(), 1240, 800)
	u.llmChip.Set("Колибри · qwen3-coder-30b-a3b · 64k", toneGood)
	u.ghChip.Set("GitHub · zigorminsk-debug", toneGood)
	snap(t, win, "03-ready")

	play(u, pipeline.StageGenerate)
	u.runBtn.SetText("Остановить")
	u.tick()
	if u.tabs.SelectedIndex() != 1 {
		t.Errorf("generation should bring the model tab forward, got tab %d", u.tabs.SelectedIndex())
	}
	if got := u.stream.answer.String(); got != demoReply {
		t.Errorf("stream answer differs from what the model sent (%d vs %d bytes)", len(got), len(demoReply))
	}
	snap(t, win, "04-generating")

	play(u, pipeline.StageBuild)
	u.tick()
	if u.stages.states[3] != stageActive || u.stages.states[2] != stageDone || u.stages.states[4] != stagePending {
		t.Errorf("stage states = %v", u.stages.states)
	}
	snap(t, win, "05-building")
	u.tabs.SelectIndex(3)
	snap(t, win, "06-files")
	u.tabs.SelectIndex(0)
	snap(t, win, "07-journal")

	// 3. Outcomes.
	observer{u}.Build(demoRuns(true, true))
	observer{u}.Log(pipeline.Good, "Сборка №7 прошла за 3 мин 12 с")
	observer{u}.Stage(pipeline.StageDeliver, 1, 4)
	observer{u}.Log(pipeline.Good, "Релиз опубликован: https://github.com/zigorminsk-debug/calc/releases/tag/build-7")
	out := pipeline.Outcome{
		RepoURL: "https://github.com/zigorminsk-debug/calc", RunURL: "https://github.com/zigorminsk-debug/calc/actions/runs/1",
		ReleaseURL: "https://github.com/zigorminsk-debug/calc/releases/tag/build-7",
		Dir:        filepath.Join("C:", "Users", "igor", "KolibriForge", "calc", "build-7"), Attempts: 1,
	}
	u.finishRun(out, nil)
	if !u.banner.Visible() || u.running || u.runBtn.Text != "Запустить" || u.repo.Disabled() {
		t.Errorf("after success: banner=%v running=%v button=%q", u.banner.Visible(), u.running, u.runBtn.Text)
	}
	if u.stages.states[4] != stageDone {
		t.Errorf("stage states after success = %v", u.stages.states)
	}
	snap(t, win, "08-success")

	u.tabs.SelectIndex(4)
	observer{u}.Build(demoRuns(true, false))
	u.running = true
	u.stage = pipeline.StageBuild
	u.finishRun(pipeline.Outcome{RunURL: out.RunURL, RepoURL: out.RepoURL, Attempts: 4}, &pipeline.BuildFailedError{Attempts: 4, RunURL: out.RunURL})
	if u.stages.states[3] != stageFailed {
		t.Errorf("stage states after failure = %v", u.stages.states)
	}
	snap(t, win, "09-build-failed")

	u.running = true
	u.stage = pipeline.StageGenerate
	u.finishRun(pipeline.Outcome{}, context.Canceled)
	snap(t, win, "10-stopped")

	// 4. Settings.
	u.showSettings()
	snap(t, win, "11-settings")

	// 5. A small window must stay usable.
	u, win = newTestUI(t, demoConfig(), 1000, 640)
	u.llmChip.Set("Колибри · qwen3-coder-30b-a3b · 64k", toneGood)
	u.ghChip.Set("GitHub · zigorminsk-debug · нет workflow", toneBusy)
	play(u, pipeline.StageBuild)
	u.tick()
	snap(t, win, "12-small-window")

	// The window must fit a 1024×768 screen even with the longest labels.
	u.llmChip.Set("Колибри · "+shorten("a-very-long-model-identifier-q8-k-m", 20)+" · 128k", toneGood)
	u.ghChip.Set("GitHub · "+shorten("a-very-long-github-login", 16)+" · нет workflow", toneBusy)
	// (In a window this narrow the systems take two rows.)
	if min := win.Content().MinSize(); min.Width > 1000 || min.Height > 680 {
		t.Errorf("minimum window size is %v, want at most 1000×680", min)
	}
	t.Logf("minimum window size: %v", win.Content().MinSize())
	if u.targetFlow.rows != 2 {
		t.Errorf("systems are in %d rows in the small window, want 2", u.targetFlow.rows)
	}
}

func TestFormValidation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	u, _ := newTestUI(t, config.Default(), 1240, 800)

	u.repo.SetText("me/app")
	if _, err := u.taskFromForm(); err == nil || !strings.Contains(err.Error(), "что нужно сделать") {
		t.Errorf("empty prompt: err = %v", err)
	}
	u.prompt.SetText("сделай")
	for _, c := range u.targets {
		c.SetChecked(false)
	}
	if _, err := u.taskFromForm(); err == nil || !strings.Contains(err.Error(), "систему") {
		t.Errorf("no targets: err = %v", err)
	}
	// A web project has no OS choice, so no targets is fine there.
	for i, k := range u.kindInfos {
		if k.Kind == "static" {
			u.kind.SetSelectedIndex(i)
		}
	}
	if !u.targets["windows"].Disabled() {
		t.Error("OS checkboxes should be disabled for a static site")
	}
	task, err := u.taskFromForm()
	if err != nil {
		t.Fatalf("static site: %v", err)
	}
	if task.Owner != "me" || task.Repo != "app" || task.Kind != "static" || task.MaxFixes != 3 || !task.Private || !task.Release {
		t.Errorf("task = %+v", task)
	}

	// Without a token the run must not start.
	u.onRunTapped()
	if u.running || !strings.Contains(u.problem.Text, "токен") {
		t.Errorf("no token: running=%v problem=%q", u.running, u.problem.Text)
	}
}

// Android is offered only where an APK can be built, and the form says what
// the choice leads to.
func TestAndroidTarget(t *testing.T) {
	u, win := newTestUI(t, demoConfig(), 1240, 800)
	u.repo.SetText("me/app")
	u.prompt.SetText("сделай")
	u.targets["macos"].SetChecked(false)
	// At the usual width the four systems fit one row, and stay there.
	u.tick()
	if u.targetFlow.rows != 1 || u.targetFlow.stale {
		t.Errorf("systems: %d rows, stale=%v; want one settled row", u.targetFlow.rows, u.targetFlow.stale)
	}
	android := u.targets["android"]
	if android == nil {
		t.Fatal("no Android checkbox")
	}
	if android.Text != "Android" || android.Checked {
		t.Errorf("android checkbox: text=%q checked=%v", android.Text, android.Checked)
	}
	pick := func(kind string) {
		t.Helper()
		for i, k := range u.kindInfos {
			if string(k.Kind) == kind {
				u.kind.SetSelectedIndex(i)
				return
			}
		}
		t.Fatalf("no kind %q", kind)
	}

	// Auto: allowed, and the hint says which stack the model will use.
	pick("auto")
	if android.Disabled() || u.targetHint.Visible() {
		t.Errorf("auto, not ticked: disabled=%v hint=%v", android.Disabled(), u.targetHint.Visible())
	}
	android.SetChecked(true)
	if !u.targetHint.Visible() || !strings.Contains(u.targetHint.Text, "Go + Fyne") || !strings.Contains(u.targetHint.Text, "APK") {
		t.Errorf("auto: hint=%q visible=%v", u.targetHint.Text, u.targetHint.Visible())
	}
	task, err := u.taskFromForm()
	if err != nil || !reflect.DeepEqual(task.Targets, []string{"windows", "linux", "android"}) {
		t.Errorf("auto: targets=%v err=%v", task.Targets, err)
	}

	snap(t, win, "40-android-auto")

	pick("go-fyne")
	if android.Disabled() || !u.targetHint.Visible() || strings.Contains(u.targetHint.Text, "модель напишет") {
		t.Errorf("go-fyne: disabled=%v hint=%q", android.Disabled(), u.targetHint.Text)
	}
	snap(t, win, "41-android-fyne")

	// A stack with no APK: the box is greyed out and the target is left out.
	pick("rust")
	if !android.Disabled() || u.targets["linux"].Disabled() || !strings.Contains(u.targetHint.Text, "недоступен") {
		t.Errorf("rust: android disabled=%v hint=%q", android.Disabled(), u.targetHint.Text)
	}
	snap(t, win, "42-android-unavailable")
	task, err = u.taskFromForm()
	if err != nil || !reflect.DeepEqual(task.Targets, []string{"windows", "linux"}) {
		t.Errorf("rust: targets=%v err=%v", task.Targets, err)
	}
	u.targets["windows"].SetChecked(false)
	u.targets["linux"].SetChecked(false)
	if _, err := u.taskFromForm(); err == nil || !strings.Contains(err.Error(), "Android") {
		t.Errorf("rust, android only: err = %v", err)
	}

	// The tick is remembered whatever the stack, and comes back with it.
	var saved config.Config
	u.Save = func(c config.Config) error { saved = c; return nil }
	u.Stop()
	if !reflect.DeepEqual(saved.Targets, []string{"android"}) {
		t.Errorf("saved targets = %v", saved.Targets)
	}
	pick("go-fyne")
	if android.Disabled() {
		t.Error("Android must come back for Go + Fyne")
	}
	android.SetChecked(false)
	if u.targetHint.Visible() {
		t.Error("the hint must go away with the tick")
	}
}

func TestFormSurvivesRestart(t *testing.T) {
	u, _ := newTestUI(t, config.Default(), 1240, 800)
	var saved config.Config
	u.Save = func(c config.Config) error { saved = c; return nil }
	u.repo.SetText("me/app")
	u.branch.SetText("dev")
	u.prompt.SetText("задача")
	u.fixes.SetSelectedIndex(5)
	u.targets["macos"].SetChecked(true)
	u.release.SetChecked(false)
	u.Stop()

	u2, _ := newTestUI(t, saved, 1240, 800)
	if u2.repo.Text != "me/app" || u2.branch.Text != "dev" || u2.prompt.Text != "задача" || u2.fixes.SelectedIndex() != 5 ||
		!u2.targets["macos"].Checked || u2.release.Checked {
		t.Errorf("restored form: %+v", saved)
	}
}

func TestGroupDigitsAndClock(t *testing.T) {
	if got := groupDigits(1234567); got != "1\u00a0234\u00a0567" {
		t.Errorf("groupDigits = %q", got)
	}
	if got := groupDigits(999); got != "999" {
		t.Errorf("groupDigits = %q", got)
	}
	if got := clock(4*time.Minute + 12*time.Second); got != "04:12" {
		t.Errorf("clock = %q", got)
	}
	if got := clock(time.Hour + 61*time.Second); got != "1:01:01" {
		t.Errorf("clock = %q", got)
	}
}

// ---- finding widgets inside open dialogs --------------------------------

// walk visits every object of the window, overlays (dialogs) included.
func walk(u *UI, visit func(fyne.CanvasObject)) {
	var rec func(o fyne.CanvasObject)
	rec = func(o fyne.CanvasObject) {
		if o == nil {
			return
		}
		visit(o)
		switch c := o.(type) {
		case *fyne.Container:
			for _, child := range c.Objects {
				rec(child)
			}
		case fyne.Widget:
			for _, child := range test.WidgetRenderer(c).Objects() {
				rec(child)
			}
		}
	}
	canvas := u.win.Canvas()
	for _, o := range canvas.Overlays().List() {
		rec(o)
	}
}

func findButton(t *testing.T, u *UI, label string) *widget.Button {
	t.Helper()
	var found *widget.Button
	walk(u, func(o fyne.CanvasObject) {
		if b, ok := o.(*widget.Button); ok && b.Text == label && b.Visible() {
			found = b
		}
	})
	if found == nil {
		t.Fatalf("no button %q in the open dialogs", label)
	}
	return found
}

func findEntry(t *testing.T, u *UI, placeholder string) *widget.Entry {
	t.Helper()
	var found *widget.Entry
	walk(u, func(o fyne.CanvasObject) {
		if e, ok := o.(*widget.Entry); ok && e.PlaceHolder == placeholder {
			found = e
		}
	})
	if found == nil {
		t.Fatalf("no entry %q in the open dialogs", placeholder)
	}
	return found
}

func findList(t *testing.T, u *UI) *widget.List {
	t.Helper()
	var found *widget.List
	walk(u, func(o fyne.CanvasObject) {
		if l, ok := o.(*widget.List); ok {
			found = l
		}
	})
	if found == nil {
		t.Fatal("no list in the open dialogs")
	}
	return found
}

// A reply that broke off and a stage with work left over are not failures:
// the banner says what was saved and how to continue, and every line of
// the journal lands in the log file.
func TestUnfinishedWorkAndLogFile(t *testing.T) {
	u, win := newTestUI(t, demoConfig(), 1240, 800)
	logPath := filepath.Join(t.TempDir(), "kolibri-forge.log")
	u.Log = runlog.New(logPath)
	u.journal.logDir = filepath.Dir(logPath)
	var opened string
	u.journal.openDir = func(dir string) { opened = dir }

	play(u, pipeline.StageGenerate)
	observer{u}.Log(pipeline.Warn, "Ответ модели прервался: Колибри оборвал ответ на середине — сервер остановлен или перезапущен?")
	observer{u}.Log(pipeline.Good, "Коммит 3f9c2ab в zigorminsk-debug/calc (main) (без сборки): 6 файлов — wip: Каркас калькулятора")
	out := pipeline.Outcome{
		RepoURL: "https://github.com/zigorminsk-debug/calc", Branch: "main", Attempts: 1,
		LogURL: "https://github.com/zigorminsk-debug/calc/blob/main/.kolibri/worklog.md",
	}
	u.finishRun(out, &pipeline.InterruptedError{Err: errors.New("Колибри оборвал ответ на середине — сервер остановлен или перезапущен?"), Saved: 6})
	if !u.banner.Visible() || u.running || u.runBtn.Text != "Запустить" {
		t.Errorf("after an interrupted reply: banner=%v running=%v button=%q", u.banner.Visible(), u.running, u.runBtn.Text)
	}
	if got := bannerText(u); !strings.Contains(got, "Ответ модели прервался") || !strings.Contains(got, "Готовые файлы (6) сохранены") {
		t.Errorf("banner = %q", got)
	}
	snap(t, win, "30-interrupted")

	if !u.journal.fileBtn.Visible() {
		t.Fatal("the journal must offer its file on disk")
	}
	test.Tap(u.journal.fileBtn)
	if opened != filepath.Dir(logPath) {
		t.Errorf("opened %q", opened)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"! Ответ модели прервался: Колибри оборвал", "✓ Коммит 3f9c2ab", "запустите задачу снова"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log file lacks %q:\n%s", want, data)
		}
	}

	// A green stage of an unfinished project.
	u.running = true
	out.Next = "- экран истории\n- сохранение в файл"
	out.Dir = filepath.Join("C:", "Users", "igor", "KolibriForge", "calc", "build-8")
	out.RunURL = "https://github.com/zigorminsk-debug/calc/actions/runs/1008"
	u.finishRun(out, nil)
	if got := bannerText(u); !strings.Contains(got, "Этап собран, работа не окончена") || !strings.Contains(got, "запустите задачу снова") {
		t.Errorf("banner = %q", got)
	}
	snap(t, win, "31-stage-done")
}

// bannerText collects the text of the result banner.
func bannerText(u *UI) string {
	var b strings.Builder
	var rec func(o fyne.CanvasObject)
	rec = func(o fyne.CanvasObject) {
		switch v := o.(type) {
		case *canvas.Text:
			b.WriteString(v.Text + "\n")
		case *widget.Label:
			b.WriteString(v.Text + "\n")
		case *fyne.Container:
			for _, child := range v.Objects {
				rec(child)
			}
		case fyne.Widget:
			for _, child := range test.WidgetRenderer(v).Objects() {
				rec(child)
			}
		}
	}
	rec(u.banner)
	return b.String()
}

func TestWebResearchViewAndSettings(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	u, win := newTestUI(t, demoConfig(), 1240, 800)
	if !u.useWeb.Checked {
		t.Fatal("web search must be enabled by default")
	}
	task, err := u.taskFromForm()
	if err != nil {
		t.Fatal(err)
	}
	if !task.UseWeb || task.MaxWebSteps != 8 {
		t.Errorf("task web fields = UseWeb:%v MaxWebSteps:%d", task.UseWeb, task.MaxWebSteps)
	}

	// Simulate Lightpanda executing search + page read + cached lookup.
	steps := []pipeline.WebStep{
		{
			Index: 1, Round: 1, Attempt: 1,
			Action:   lightpanda.Action{Kind: "search", Query: "radmin vpn architecture"},
			Content:  "1. Radmin VPN — https://www.radmin-vpn.com/\n   Virtual local network.",
			Duration: 420 * time.Millisecond,
		},
		{
			Index: 2, Round: 2, Attempt: 1,
			Action:  lightpanda.Action{Kind: "open", URL: "https://www.radmin-vpn.com/"},
			URL:     "https://www.radmin-vpn.com/",
			Title:   "Radmin VPN",
			Running: true,
		},
	}
	observer{u}.Web(steps)
	if u.webTab.Text != "Интернет (2)" || u.web.empty.Visible() {
		t.Fatalf("webTab=%q empty=%v", u.webTab.Text, u.web.empty.Visible())
	}
	if !u.web.openBtn.Visible() || !strings.Contains(u.web.curText, "выполняет действие") {
		t.Errorf("running step: openBtn=%v text=%q", u.web.openBtn.Visible(), u.web.curText)
	}

	// Complete step 2 and append cached step 3.
	steps[1].Running = false
	steps[1].Content = "# Radmin VPN\nEncrypted peer-to-peer mesh over UDP."
	steps[1].Duration = 650 * time.Millisecond
	steps = append(steps, pipeline.WebStep{
		Index: 3, Round: 3, Attempt: 1,
		Action:  lightpanda.Action{Kind: "search", Query: "radmin vpn architecture"},
		Content: steps[0].Content,
		Cached:  true,
	})
	observer{u}.Web(steps)
	u.tabs.SelectIndex(2)
	if u.webTab.Text != "Интернет (3)" {
		t.Errorf("webTab = %q", u.webTab.Text)
	}
	u.web.list.Select(1)
	if !strings.Contains(u.web.curText, "Encrypted peer-to-peer mesh") || !u.web.openBtn.Visible() {
		t.Errorf("step 2 preview = %q (openBtn=%v)", u.web.curText, u.web.openBtn.Visible())
	}
	test.Tap(u.web.copyBtn)
	if !strings.Contains(u.app.Clipboard().Content(), "Encrypted peer-to-peer mesh") {
		t.Errorf("clipboard = %q", u.app.Clipboard().Content())
	}
	snap(t, win, "32-web-research")

	// Result banner records how many browser steps were run.
	u.running = true
	u.finishRun(pipeline.Outcome{
		RepoURL:  "https://github.com/zigorminsk-debug/localvpn",
		Dir:      filepath.Join("C:", "Users", "igor", "KolibriForge", "localvpn", "build-1"),
		Attempts: 1,
		WebSteps: 3,
	}, nil)
	if got := bannerText(u); !strings.Contains(got, "3 действия в интернете") {
		t.Errorf("banner missing web count: %q", got)
	}

	// Settings dialog validates ws:// vs http:// MCP URL and saves web options.
	var saved config.Config
	u.Save = func(c config.Config) error { saved = c; return nil }
	u.useWeb.SetChecked(false)
	u.showSettings()
	mcpEntry := findEntry(t, u, "http://127.0.0.1:9222 — не обязательно (lightpanda mcp --port 9222)")
	mcpEntry.SetText("ws://127.0.0.1:9222")
	test.Tap(findButton(t, u, "Сохранить"))
	if saved.LightpandaURL != "" {
		t.Fatal("ws:// CDP address should have been rejected")
	}
	mcpEntry.SetText("http://127.0.0.1:9222")
	test.Tap(findButton(t, u, "Сохранить"))
	u.bg.Wait()
	if saved.LightpandaURL != "http://127.0.0.1:9222/mcp" && saved.LightpandaURL != "http://127.0.0.1:9222" {
		t.Errorf("saved LightpandaURL = %q", saved.LightpandaURL)
	}
	if saved.UseWeb {
		t.Error("unchecked useWeb should persist as false")
	}

	// One-click Lightpanda installation and verification from Settings.
	if runtime.GOOS != "windows" {
		asset := lightpanda.AssetName(runtime.GOOS, runtime.GOARCH)
		if asset != "" {
			relSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/lightpanda-io/browser/releases":
					_ = json.NewEncoder(w).Encode([]map[string]any{{
						"tag_name": "1.0.4",
						"assets":   []map[string]any{{"name": asset, "browser_download_url": "http://" + r.Host + "/bin", "size": 58}},
					}})
				case "/bin":
					w.Write([]byte("#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 1.0.4; exit 0; fi\n"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer relSrv.Close()

			u.cfg.LightpandaURL = ""
			u.lpManagedDir = t.TempDir()
			u.lpReleasesURL = relSrv.URL
			u.showSettings()
			test.Tap(findButton(t, u, "Установить Lightpanda"))
			u.bg.Wait()
			var sawInstalled bool
			walk(u, func(o fyne.CanvasObject) {
				if l, ok := o.(*widget.Label); ok && strings.Contains(l.Text, "Установлено: Lightpanda 1.0.4") {
					sawInstalled = true
				}
			})
			if !sawInstalled {
				t.Error("Settings did not show installed Lightpanda 1.0.4 confirmation")
			}
			test.Tap(findButton(t, u, "Проверить Lightpanda"))
			u.bg.Wait()
			var sawChecked bool
			walk(u, func(o fyne.CanvasObject) {
				if l, ok := o.(*widget.Label); ok && strings.HasPrefix(l.Text, "Lightpanda 1.0.4 (") {
					sawChecked = true
				}
			})
			if !sawChecked {
				t.Error("Settings check button did not report Lightpanda 1.0.4")
			}
		}
	}
}
