// Package ui is the Fyne window of Kolibri Forge. It imports no driver, so
// the same code runs under the real desktop driver and under Fyne's
// software renderer in tests.
package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/config"
	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/pipeline"
	"github.com/zigorminsk-debug/kolibri/internal/runlog"
	"github.com/zigorminsk-debug/kolibri/internal/workflow"
)

// AppName is the product name shown in the window.
const AppName = "Kolibri Forge"

// UI is the main window's content and behaviour.
type UI struct {
	app fyne.App
	win fyne.Window
	cfg config.Config
	// Save persists settings; replaced in tests.
	Save func(config.Config) error

	// Test hooks: stand-ins for api.github.com and github.com.
	ghBaseURL    string
	oauthBaseURL string

	// header
	llmChip *chip
	ghChip  *chip

	// dialogs currently open (kept for tests)
	authDialog *dialog.CustomDialog
	repoDialog *dialog.CustomDialog

	// task form
	repo      *widget.Entry
	repoPick  *widget.Button
	branch    *widget.Entry
	kind      *widget.Select
	targets   map[string]*widget.Check
	targetRow *fyne.Container
	prompt    *widget.Entry
	fixes     *widget.Select
	private   *widget.Check
	release   *widget.Check
	problem   *widget.Label
	formFoot  *fyne.Container
	formBody  *fyne.Container
	runBtn    *widget.Button

	// progress
	stages  *stageBar
	attempt *canvas.Text
	timer   *canvas.Text
	banner  *fyne.Container
	head    *fyne.Container
	body    *fyne.Container
	tabs    *container.AppTabs

	journal *journal
	stream  *streamView
	files   *filesView
	build   *buildView

	// run state (UI goroutine only, except cancel which is guarded)
	mu       sync.Mutex
	cancel   context.CancelFunc
	running  bool
	started  time.Time
	stage    pipeline.Stage
	stopTick chan struct{}

	// Log is the application's log file; nil writes nothing. Set it
	// before Start.
	Log *runlog.Log

	// runUI replaces fyne.Do in tests, where the driver has no UI goroutine
	// and updates from parallel requests would otherwise overlap.
	runUI func(func())
	// bg counts background requests, so tests can wait for them.
	bg sync.WaitGroup
	// deviceSleep replaces the pause between sign-in polls in tests.
	deviceSleep func(context.Context, time.Duration) error

	// what the connection checks found
	models []colibri.Model
	llmOK  bool
	ghOK   bool
	// ghProblem is why GitHub needs a new sign-in ("" when it does not).
	ghProblem string
	checkSeq  int
	kindInfos []workflow.Info
}

// New builds the window content. Call Start afterwards to begin the
// background connection checks.
func New(app fyne.App, win fyne.Window, cfg config.Config) *UI {
	u := &UI{app: app, win: win, cfg: cfg, kindInfos: workflow.Kinds()}
	u.Save = func(c config.Config) error { return c.Save() }
	win.SetContent(u.layout())
	u.journal.sink = func(level pipeline.Level, msg string, at time.Time) {
		u.Log.Line(at, levelMarks[level], msg)
	}
	u.loadForm()
	return u
}

// do hands a UI update from a background request to the UI goroutine.
func (u *UI) do(fn func()) {
	if u.runUI != nil {
		u.runUI(fn)
		return
	}
	fyne.Do(fn)
}

// spawn runs a network request off the UI goroutine.
func (u *UI) spawn(fn func()) {
	u.bg.Add(1)
	go func() {
		defer u.bg.Done()
		fn()
	}()
}

// Start runs the connection checks and the redraw ticker.
func (u *UI) Start() {
	u.CheckConnections()
	u.stopTick = make(chan struct{})
	go func() {
		ticker := time.NewTicker(streamInterval)
		defer ticker.Stop()
		for {
			select {
			case <-u.stopTick:
				return
			case <-ticker.C:
				fyne.Do(u.tick)
			}
		}
	}()
}

// Stop cancels a running task and the ticker; call when the window closes.
func (u *UI) Stop() {
	u.mu.Lock()
	if u.cancel != nil {
		u.cancel()
	}
	u.mu.Unlock()
	if u.stopTick != nil {
		close(u.stopTick)
		u.stopTick = nil
	}
	u.saveForm()
}

// tick redraws what changes with time: the streamed text and the clock.
func (u *UI) tick() {
	u.stream.flush()
	if u.running {
		u.timer.Text = clock(time.Since(u.started))
		u.timer.Refresh()
	}
}

// ---- layout -------------------------------------------------------------

func (u *UI) layout() fyne.CanvasObject {
	bg := canvas.NewRectangle(colInk)
	body := container.NewHSplit(u.formPanel(), u.progressPanel())
	body.Offset = 0.33
	content := container.NewBorder(u.header(), nil, nil, nil, body)
	return container.NewStack(bg, container.New(layout.NewCustomPaddedLayout(10, 12, 14, 14), content))
}

func (u *UI) header() fyne.CanvasObject {
	logo := canvas.NewImageFromResource(mark)
	logo.FillMode = canvas.ImageFillContain
	title := text(AppName, 19, colText, fyne.TextStyle{Bold: true})
	// The tagline gives way first when the window is narrow.
	sub := widget.NewLabel("задача → код → GitHub Actions → программа")
	sub.Importance = widget.LowImportance
	sub.Truncation = fyne.TextTruncateEllipsis

	u.llmChip = newChip("Колибри · проверка…", toneBusy, u.CheckConnections)
	u.ghChip = newChip("GitHub · проверка…", toneBusy, func() {
		// A chip that reports a token problem leads straight to the cure.
		if u.ghProblem != "" && !u.running {
			u.showAuth(u.ghProblem, nil)
			return
		}
		u.CheckConnections()
	})
	settings := widget.NewButtonWithIcon("Настройки", theme.SettingsIcon(), u.showSettings)

	left := container.NewHBox(fixed(34, 34, logo), gap(4, 0), container.NewCenter(title))
	right := container.NewHBox(container.NewCenter(u.llmChip), gap(2, 0), container.NewCenter(u.ghChip), gap(6, 0), container.NewCenter(settings))
	bar := container.NewBorder(nil, nil, left, right, container.NewVBox(layout.NewSpacer(), sub, layout.NewSpacer()))
	u.llmChip.onResize = bar.Refresh
	u.ghChip.onResize = bar.Refresh
	return container.New(layout.NewCustomPaddedLayout(0, 12, 2, 0), bar)
}

func (u *UI) formPanel() fyne.CanvasObject {
	u.repo = widget.NewEntry()
	u.repo.SetPlaceHolder("владелец/название")
	u.repo.OnChanged = func(string) { u.clearProblem() }
	u.repoPick = iconButton(theme.SearchIcon(), u.showRepoPicker)
	u.repo.ActionItem = u.repoPick
	u.branch = widget.NewEntry()
	u.branch.SetPlaceHolder("основная")

	titles := make([]string, len(u.kindInfos))
	for i, k := range u.kindInfos {
		titles[i] = k.Title
	}
	u.kind = widget.NewSelect(titles, func(string) { u.syncTargets() })

	u.targets = map[string]*widget.Check{}
	u.targetRow = container.NewHBox()
	for _, t := range workflow.TargetNames() {
		c := widget.NewCheck(targetTitle(t), func(bool) { u.clearProblem() })
		u.targets[t] = c
		u.targetRow.Add(c)
	}

	u.prompt = widget.NewMultiLineEntry()
	u.prompt.Wrapping = fyne.TextWrapWord
	u.prompt.SetPlaceHolder("Например: оконный калькулятор с историей вычислений и тёмной темой")
	u.prompt.OnChanged = func(string) { u.clearProblem() }

	u.fixes = widget.NewSelect([]string{"не исправлять", "1 раз", "2 раза", "3 раза", "4 раза", "5 раз"}, nil)
	u.private = widget.NewCheck("Новый репозиторий — приватный", nil)
	u.release = widget.NewCheck("Публиковать релиз после сборки", nil)

	u.problem = widget.NewLabel("")
	u.problem.Importance = widget.DangerImportance
	u.problem.Wrapping = fyne.TextWrapWord
	u.problem.Hide()

	u.runBtn = widget.NewButtonWithIcon("Запустить", theme.MediaPlayIcon(), u.onRunTapped)
	u.runBtn.Importance = widget.HighImportance

	repoRow := container.NewBorder(nil, nil, nil,
		container.NewVBox(fieldLabel("Ветка"), fixed(118, u.branch.MinSize().Height, u.branch)),
		container.NewVBox(fieldLabel("Репозиторий на GitHub"), u.repo))

	top := container.NewVBox(
		text("Новая задача", 17, colText, fyne.TextStyle{Bold: true}),
		gap(0, 2),
		repoRow,
		fieldLabel("Тип проекта"),
		u.kind,
		fieldLabel("Собрать для"),
		u.targetRow,
		fieldLabel("Что нужно сделать"),
	)
	u.formFoot = container.NewVBox(
		gap(0, 2),
		container.NewBorder(nil, nil, container.NewCenter(text("Чинить упавшую сборку", 13.5, colText, fyne.TextStyle{})), fixed(130, u.fixes.MinSize().Height, u.fixes)),
		u.private,
		u.release,
		u.problem,
		gap(0, 2),
		u.runBtn,
	)
	u.formBody = container.NewBorder(top, u.formFoot, nil, nil, u.prompt)
	return panel(u.formBody)
}

func (u *UI) progressPanel() fyne.CanvasObject {
	u.stages = newStageBar("Подготовка", "Код от модели", "Коммит", "Сборка", "Программа")
	u.attempt = text("", 13, colMuted, fyne.TextStyle{})
	u.attempt.Alignment = fyne.TextAlignTrailing
	u.timer = text("00:00", 20, colFaint, fyne.TextStyle{Monospace: true, Bold: true})
	u.timer.Alignment = fyne.TextAlignTrailing
	clockBox := container.NewVBox(gap(0, 6), u.timer, u.attempt)
	top := container.NewBorder(nil, nil, nil, container.New(layout.NewCustomPaddedLayout(0, 0, 8, 6), fixed(112, 56, clockBox)), u.stages)

	u.banner = container.NewStack()
	u.banner.Hide()

	u.journal = newJournal(u.copyText)
	u.journal.openDir = u.openFolder
	u.stream = newStreamView(u.copyText)
	u.files = newFilesView()
	u.build = newBuildView(u.openURL)

	u.tabs = container.NewAppTabs(
		container.NewTabItem("Журнал", u.journal.root),
		container.NewTabItem("Ответ модели", u.stream.root),
		container.NewTabItem("Файлы", u.files.root),
		container.NewTabItem("Сборка", u.build.root),
	)
	u.head = container.NewVBox(top, u.banner, gap(0, 2))
	u.body = container.NewBorder(u.head, nil, nil, nil, u.tabs)
	return panel(u.body)
}

func targetTitle(t string) string {
	switch t {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "macos":
		return "macOS"
	}
	return t
}

// ---- form ---------------------------------------------------------------

func (u *UI) loadForm() {
	c := u.cfg
	u.repo.SetText(c.Repo)
	u.branch.SetText(c.Branch)
	u.kind.SetSelectedIndex(0)
	for i, k := range u.kindInfos {
		if string(k.Kind) == c.ProjectKind {
			u.kind.SetSelectedIndex(i)
		}
	}
	for name, check := range u.targets {
		check.SetChecked(false)
		for _, t := range c.Targets {
			if t == name {
				check.SetChecked(true)
			}
		}
	}
	u.prompt.SetText(c.LastTask)
	fix := c.MaxFixes
	if fix > 5 {
		fix = 5
	}
	u.fixes.SetSelectedIndex(fix)
	u.private.SetChecked(c.Private)
	u.release.SetChecked(c.Release)
	u.syncTargets()
	u.clearProblem()
}

// formToConfig copies the form into the settings (without saving).
func (u *UI) formToConfig() {
	u.cfg.Repo = strings.TrimSpace(u.repo.Text)
	u.cfg.Branch = strings.TrimSpace(u.branch.Text)
	u.cfg.ProjectKind = string(u.selectedKind().Kind)
	u.cfg.Targets = u.selectedTargets()
	u.cfg.LastTask = u.prompt.Text
	u.cfg.MaxFixes = max(u.fixes.SelectedIndex(), 0)
	u.cfg.Private = u.private.Checked
	u.cfg.Release = u.release.Checked
}

func (u *UI) saveForm() {
	u.formToConfig()
	if err := u.Save(u.cfg); err != nil {
		fyne.LogError("saving settings", err)
	}
}

func (u *UI) selectedKind() workflow.Info {
	i := u.kind.SelectedIndex()
	if i < 0 || i >= len(u.kindInfos) {
		return u.kindInfos[0]
	}
	return u.kindInfos[i]
}

func (u *UI) selectedTargets() []string {
	var out []string
	for _, t := range workflow.TargetNames() {
		if u.targets[t].Checked {
			out = append(out, t)
		}
	}
	return out
}

// syncTargets greys out the OS choice for stacks whose output is the same
// everywhere (a web build), so the form never offers a meaningless option.
func (u *UI) syncTargets() {
	perOS := u.selectedKind().PerOS
	for _, c := range u.targets {
		if perOS && !u.running {
			c.Enable()
		} else {
			c.Disable()
		}
	}
	u.clearProblem()
}

func (u *UI) showProblem(msg string) {
	u.problem.SetText(capitalize(msg))
	u.problem.Show()
	u.relayoutForm()
}

func (u *UI) clearProblem() {
	if u.problem != nil && u.problem.Visible() {
		u.problem.Hide()
		u.relayoutForm()
	}
}

// relayoutForm makes room for the message above the run button (or takes it
// back), at the expense of the task text.
func (u *UI) relayoutForm() {
	if u.formBody == nil {
		return
	}
	u.formFoot.Refresh()
	u.formBody.Refresh()
}

func (u *UI) setFormEnabled(on bool) {
	type toggler interface {
		Enable()
		Disable()
	}
	controls := []toggler{u.repo, u.repoPick, u.branch, u.kind, u.prompt, u.fixes, u.private, u.release}
	for _, c := range controls {
		if on {
			c.Enable()
		} else {
			c.Disable()
		}
	}
	u.syncTargets()
}

// ---- running a task -----------------------------------------------------

func (u *UI) onRunTapped() {
	if u.running {
		u.mu.Lock()
		if u.cancel != nil {
			u.cancel()
		}
		u.mu.Unlock()
		u.runBtn.SetText("Останавливаю…")
		u.runBtn.Disable()
		return
	}
	task, err := u.taskFromForm()
	if err != nil {
		u.showProblem(err.Error())
		return
	}
	if u.cfg.Token() == "" {
		u.showProblem("Нужен токен GitHub: без него код не отправить.")
		u.showAuth("Чтобы отправить код в GitHub, войдите в него.", nil)
		return
	}
	u.saveForm()
	u.startRun(task)
}

func (u *UI) taskFromForm() (pipeline.Task, error) {
	owner, name, err := config.SplitRepo(u.repo.Text)
	if err != nil {
		return pipeline.Task{}, err
	}
	if strings.TrimSpace(u.prompt.Text) == "" {
		return pipeline.Task{}, errors.New("опишите, что нужно сделать")
	}
	kind := u.selectedKind()
	targets := u.selectedTargets()
	if kind.PerOS && len(targets) == 0 {
		return pipeline.Task{}, errors.New("отметьте хотя бы одну систему, для которой собирать программу")
	}
	return pipeline.Task{
		Prompt:      u.prompt.Text,
		Owner:       owner,
		Repo:        name,
		Branch:      strings.TrimSpace(u.branch.Text),
		Kind:        kind.Kind,
		Targets:     targets,
		MaxFixes:    max(u.fixes.SelectedIndex(), 0),
		Private:     u.private.Checked,
		Release:     u.release.Checked,
		OutputDir:   u.cfg.OutputDir,
		Model:       u.cfg.Model,
		Temperature: u.cfg.Temperature,
		MaxTokens:   u.cfg.MaxTokens,
	}, nil
}

func (u *UI) clients() (*colibri.Client, *ghapi.Client) {
	llm := &colibri.Client{
		BaseURL:     u.cfg.ColibriURL,
		APIKey:      u.cfg.ColibriKey,
		IdleTimeout: time.Duration(u.cfg.IdleTimeoutMin) * time.Minute,
	}
	return llm, u.github(u.cfg.Token())
}

func (u *UI) startRun(task pipeline.Task) {
	ctx, cancel := context.WithCancel(context.Background())
	u.mu.Lock()
	u.cancel = cancel
	u.mu.Unlock()

	u.running = true
	u.started = time.Now()
	u.journal.clear()
	u.stream.reset()
	u.files.set(nil)
	u.build.clear()
	u.hideBanner()
	u.attempt.Text = ""
	u.attempt.Refresh()
	u.timer.Color = colText
	u.tabs.SelectIndex(0)
	u.setFormEnabled(false)
	u.runBtn.SetText("Остановить")
	u.runBtn.SetIcon(theme.MediaStopIcon())
	u.runBtn.Importance = widget.DangerImportance
	u.runBtn.Refresh()

	u.Log.Start(u.started, task.Owner+"/"+task.Repo, task.Branch, task.Model, task.Prompt)
	u.journal.logDir = ""
	if p := u.Log.Path(); p != "" {
		u.journal.logDir = filepath.Dir(p)
	}

	llm, gh := u.clients()
	runner := &pipeline.Runner{LLM: llm, GH: gh, Obs: observer{u}}
	go func() {
		out, err := runner.Run(ctx, task)
		cancel()
		fyne.Do(func() { u.finishRun(out, err) })
	}()
}

func (u *UI) finishRun(out pipeline.Outcome, err error) {
	u.mu.Lock()
	u.cancel = nil
	u.mu.Unlock()
	u.running = false
	u.stream.flush()
	u.setFormEnabled(true)
	u.runBtn.SetText("Запустить")
	u.runBtn.SetIcon(theme.MediaPlayIcon())
	u.runBtn.Importance = widget.HighImportance
	u.runBtn.Enable()
	u.runBtn.Refresh()
	u.timer.Text = clock(time.Since(u.started))
	u.timer.Refresh()

	var failed *pipeline.BuildFailedError
	var cut *pipeline.InterruptedError
	switch {
	case err == nil:
		u.setStages(pipeline.StageDeliver+1, false)
		u.showResult(out)
	case errors.As(err, &cut):
		u.setStages(u.stage, true)
		u.journal.add(pipeline.Warn, capitalize(err.Error())+".", time.Now())
		u.showBanner(toneBusy, "Ответ модели прервался",
			fmt.Sprintf("Готовые файлы (%d) сохранены в репозитории, ход работы записан в журнал. Запустите задачу снова — модель продолжит с этого места.", cut.Saved), out, false)
	case errors.Is(err, context.Canceled):
		u.setStages(u.stage, true)
		u.journal.add(pipeline.Warn, "Остановлено. Если сборка уже началась, она продолжится на GitHub.", time.Now())
		detail := "Задача прервана. Начатое на GitHub не отменяется."
		if out.LogURL != "" {
			detail = "Задача прервана. Всё, что модель успела сдать, сохранено в репозитории и записано в журнал работы."
		}
		u.showBanner(toneBusy, "Остановлено", detail, out, false)
	case errors.As(err, &failed):
		u.setStages(pipeline.StageBuild, true)
		u.journal.add(pipeline.Bad, capitalize(err.Error())+".", time.Now())
		u.showBanner(toneBad, capitalize(err.Error()), "Журнал ошибок — на вкладке «Журнал». Уточните задачу или увеличьте число исправлений и запустите снова.", out, false)
	default:
		u.setStages(u.stage, true)
		u.journal.add(pipeline.Bad, capitalize(err.Error()), time.Now())
		u.showBanner(toneBad, "Не получилось", capitalize(err.Error()), out, false)
	}
}

func capitalize(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// setStages paints the tracker: everything before current is done, current
// is active or failed, the rest is pending.
func (u *UI) setStages(current pipeline.Stage, failed bool) {
	states := make([]stageState, 5)
	for i := range states {
		switch {
		case i < int(current):
			states[i] = stageDone
		case i == int(current) && failed:
			states[i] = stageFailed
		case i == int(current):
			states[i] = stageActive
		}
	}
	u.stages.Set(states)
}

func (u *UI) showResult(out pipeline.Outcome) {
	title := "Программа собрана"
	var parts []string
	if out.Attempts > 1 {
		parts = append(parts, fmt.Sprintf("с %d-й попытки", out.Attempts))
	}
	if out.Dir != "" {
		parts = append(parts, "файлы в "+out.Dir)
	} else {
		parts = append(parts, "файлы — на странице сборки в GitHub")
	}
	detail := capitalize(strings.Join(parts, ", "))
	if out.Next != "" {
		title = "Этап собран, работа не окончена"
		detail += ". Модель записала в журнал работы, что осталось сделать: запустите задачу снова, чтобы продолжить."
	}
	u.showBanner(toneGood, title, detail, out, true)
}

// showBanner puts the outcome of a run above the tabs with the actions
// that make sense for it.
func (u *UI) showBanner(t tone, title, detail string, out pipeline.Outcome, ok bool) {
	c := t.color()
	bg := canvas.NewRectangle(withAlpha(c, 0x1C))
	bg.CornerRadius = 10
	bg.StrokeColor = withAlpha(c, 0x90)
	bg.StrokeWidth = 1

	head := text(title, 16, c, fyne.TextStyle{Bold: true})
	body := widget.NewLabel(detail)
	body.Wrapping = fyne.TextWrapWord

	actions := container.NewHBox()
	if ok && out.Dir != "" {
		open := widget.NewButtonWithIcon("Открыть папку", theme.FolderOpenIcon(), func() { u.openFolder(out.Dir) })
		open.Importance = widget.HighImportance
		actions.Add(open)
	}
	if out.ReleaseURL != "" {
		actions.Add(widget.NewButtonWithIcon("Релиз", theme.DownloadIcon(), func() { u.openURL(out.ReleaseURL) }))
	}
	if out.RunURL != "" {
		actions.Add(widget.NewButtonWithIcon("Сборка на GitHub", theme.ComputerIcon(), func() { u.openURL(out.RunURL) }))
	} else if out.RepoURL != "" {
		actions.Add(widget.NewButtonWithIcon("Репозиторий", theme.ComputerIcon(), func() { u.openURL(out.RepoURL) }))
	}

	textCol := container.NewVBox(container.New(layout.NewCustomPaddedLayout(4, 0, 9, 0), head), container.NewThemeOverride(body, denseTheme{}))
	row := container.NewBorder(nil, nil, nil, container.NewCenter(actions), textCol)
	u.banner.Objects = []fyne.CanvasObject{bg, container.New(layout.NewCustomPaddedLayout(6, 8, 8, 10), row)}
	u.banner.Show()
	u.relayoutHead()
}

// hideBanner removes the outcome of the previous run.
func (u *UI) hideBanner() {
	u.banner.Hide()
	u.relayoutHead()
}

// relayoutHead gives the banner its height back to (or takes it from) the tabs.
func (u *UI) relayoutHead() {
	u.head.Refresh()
	u.body.Refresh()
	u.journal.scroll.ScrollToBottom()
}

// ---- pipeline observer --------------------------------------------------

// observer forwards pipeline events to the UI goroutine.
type observer struct{ u *UI }

func (o observer) Stage(stage pipeline.Stage, attempt, attempts int) {
	fyne.Do(func() { o.u.onStage(stage, attempt, attempts) })
}

func (o observer) Log(level pipeline.Level, msg string) {
	now := time.Now()
	fyne.Do(func() { o.u.journal.add(level, msg, now) })
}

func (o observer) ModelStart(title string) {
	o.u.stream.push(streamChunk{text: title, title: true})
}

func (o observer) ModelDelta(s string, reasoning bool) {
	o.u.stream.push(streamChunk{text: s, reasoning: reasoning})
}

func (o observer) Files(files []pipeline.FileChange) {
	fyne.Do(func() { o.u.files.set(files) })
}

func (o observer) Build(runs []pipeline.RunStatus) {
	fyne.Do(func() { o.u.build.set(runs) })
}

func (u *UI) onStage(stage pipeline.Stage, attempt, attempts int) {
	u.stage = stage
	u.setStages(stage, false)
	if attempts > 1 {
		u.attempt.Text = fmt.Sprintf("попытка %d из %d", attempt, attempts)
	} else {
		u.attempt.Text = ""
	}
	u.attempt.Refresh()
	// Bring forward the tab where things are happening now.
	switch stage {
	case pipeline.StageGenerate:
		u.tabs.SelectIndex(1)
	case pipeline.StageBuild:
		u.tabs.SelectIndex(3)
	case pipeline.StageDeliver:
		u.tabs.SelectIndex(0)
	}
}

// ---- connection checks --------------------------------------------------

// CheckConnections asks colibri and GitHub who is there and shows it in
// the header chips.
func (u *UI) CheckConnections() {
	u.checkSeq++
	seq := u.checkSeq
	u.llmChip.Set("Колибри · проверка…", toneBusy)
	u.ghChip.Set("GitHub · проверка…", toneBusy)
	llm, gh := u.clients()
	wanted := u.cfg.Model

	u.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		models, err := llm.Models(ctx)
		u.do(func() {
			if seq != u.checkSeq {
				return
			}
			u.models, u.llmOK = models, err == nil && len(models) > 0
			switch {
			case err != nil:
				u.llmChip.Set("Колибри · нет связи", toneBad)
			case len(models) == 0:
				u.llmChip.Set("Колибри · нет модели", toneBad)
			default:
				m := models[0]
				for _, c := range models {
					if c.ID == wanted {
						m = c
					}
				}
				label := "Колибри · " + shorten(m.ID, 20)
				if m.ContextLength > 0 {
					label += fmt.Sprintf(" · %dk", m.ContextLength/1024)
				}
				u.llmChip.Set(label, toneGood)
			}
		})
	})

	if gh.Token == "" {
		u.ghOK = false
		u.ghProblem = "Токен GitHub не задан. Войдите, чтобы программа могла отправлять код и следить за сборкой."
		u.ghChip.Set("GitHub · войти", toneBad)
		return
	}
	u.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		user, err := gh.CurrentUser(ctx)
		u.do(func() {
			if seq != u.checkSeq {
				return
			}
			u.ghOK = err == nil
			u.ghProblem = authReason(err)
			var api *ghapi.APIError
			switch {
			case errors.As(err, &api) && api.Status == 401:
				u.ghChip.Set("GitHub · токен не принят", toneBad)
			case err != nil:
				u.ghChip.Set("GitHub · нет связи", toneBad)
			case user.Classic && !user.HasScope("workflow"):
				u.ghChip.Set("GitHub · "+shorten(user.Login, 16)+" · нет workflow", toneBusy)
			default:
				u.ghChip.Set("GitHub · "+shorten(user.Login, 22), toneGood)
			}
		})
	})
}

func shorten(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// ---- system integration -------------------------------------------------

func (u *UI) openURL(raw string) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return
	}
	if err := u.app.OpenURL(parsed); err != nil {
		u.journal.add(pipeline.Warn, "Не удалось открыть браузер: "+raw, time.Now())
	}
}

func (u *UI) openFolder(dir string) {
	if _, err := os.Stat(dir); err != nil {
		u.journal.add(pipeline.Warn, "Папка не найдена: "+dir, time.Now())
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		u.journal.add(pipeline.Warn, "Не удалось открыть папку "+dir+": "+err.Error(), time.Now())
		return
	}
	go cmd.Wait()
}

func (u *UI) copyText(s string) {
	if s == "" {
		return
	}
	u.app.Clipboard().SetContent(s)
}
