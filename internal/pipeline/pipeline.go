// Package pipeline runs one task end to end: ask colibri for code, commit
// it to GitHub, wait for the GitHub Actions build, feed a failed build back
// to the model, and download the finished program.
package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zigorminsk-debug/kolibri/internal/codeparse"
	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/workflow"
)

// Stage is a step of the pipeline as the user sees it.
type Stage int

const (
	StagePrepare Stage = iota
	StageGenerate
	StageCommit
	StageBuild
	StageDeliver
)

// Level is the severity of a journal line.
type Level int

const (
	Info Level = iota
	Good
	Warn
	Bad
)

// FileChange is one file of the current attempt, for preview.
type FileChange struct {
	Path    string
	Content string
	// Status is "new", "changed" or "deleted".
	Status string
}

// RunStatus is a workflow run with its jobs.
type RunStatus struct {
	Run  ghapi.Run
	Jobs []ghapi.Job
}

// Observer receives progress. Calls come from the pipeline's goroutine.
type Observer interface {
	// Stage reports the step being worked on; attempt counts from 1.
	Stage(stage Stage, attempt, attempts int)
	Log(level Level, text string)
	// ModelStart marks the beginning of a new reply from the model.
	ModelStart(title string)
	ModelDelta(text string, reasoning bool)
	// Files reports what the current attempt changes.
	Files(files []FileChange)
	// Build reports the state of the GitHub Actions runs.
	Build(runs []RunStatus)
}

// Task is what the user asked for.
type Task struct {
	Prompt string
	Owner  string
	Repo   string
	// Branch may be empty: the repository's default branch is used.
	Branch  string
	Kind    workflow.Kind
	Targets []string
	// MaxFixes is how many times a failed build is sent back to the model.
	MaxFixes int
	// Private applies when the repository has to be created.
	Private bool
	// Release publishes a GitHub release after a good build.
	Release bool
	// OutputDir is where artifacts are unpacked; "" skips the download.
	OutputDir string

	Model       string
	Temperature float64
	MaxTokens   int
}

// Outcome is the result of a finished run.
type Outcome struct {
	RepoURL    string
	Branch     string
	CommitSHA  string
	CommitURL  string
	RunURL     string
	ReleaseURL string
	// Dir is the folder with the downloaded build, "" when nothing was downloaded.
	Dir string
	// Artifacts are the names of the downloaded artifacts.
	Artifacts []string
	// Attempts is how many generations it took.
	Attempts int
	Kind     workflow.Kind
}

// BuildFailedError means the build is still red after every allowed attempt.
type BuildFailedError struct {
	Attempts int
	RunURL   string
}

func (e *BuildFailedError) Error() string {
	return fmt.Sprintf("сборка не прошла после %d %s", e.Attempts, plural(e.Attempts, "попытки", "попыток", "попыток"))
}

// Runner executes tasks.
type Runner struct {
	LLM *colibri.Client
	GH  *ghapi.Client
	Obs Observer

	// PollInterval is the pause between build status checks (default 6s).
	PollInterval time.Duration
	// RunStartTimeout is how long to wait for GitHub to start a run (default 3m).
	RunStartTimeout time.Duration
	// BuildTimeout bounds one build (default 60m).
	BuildTimeout time.Duration
}

func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return 6 * time.Second
}

func (r *Runner) runStartTimeout() time.Duration {
	if r.RunStartTimeout > 0 {
		return r.RunStartTimeout
	}
	return 3 * time.Minute
}

func (r *Runner) buildTimeout() time.Duration {
	if r.BuildTimeout > 0 {
		return r.BuildTimeout
	}
	return time.Hour
}

func (r *Runner) logf(level Level, format string, args ...any) {
	r.Obs.Log(level, fmt.Sprintf(format, args...))
}

// state is what a run accumulates.
type state struct {
	task     Task
	login    string
	repo     ghapi.Repo
	branch   string
	head     ghapi.Head
	modes    map[string]string // path -> git mode of files already in the repository
	project  map[string]string // path -> content of text files (existing and generated)
	opaque   map[string]bool   // existing files whose content is not loaded (binary, huge)
	model    string
	ctxLen   int
	kind     workflow.Kind
	summary  string
	attempts int
}

// Run executes a task. It returns when the build is green and downloaded,
// when it gives up, or when ctx is cancelled.
func (r *Runner) Run(ctx context.Context, task Task) (Outcome, error) {
	if strings.TrimSpace(task.Prompt) == "" {
		return Outcome{}, errors.New("опишите задачу")
	}
	if task.MaxFixes < 0 {
		task.MaxFixes = 0
	}
	st := &state{task: task, modes: map[string]string{}, project: map[string]string{}, opaque: map[string]bool{}}
	total := 1 + task.MaxFixes

	r.Obs.Stage(StagePrepare, 1, total)
	if err := r.prepare(ctx, st); err != nil {
		return Outcome{}, err
	}
	out := Outcome{RepoURL: st.repo.HTMLURL, Branch: st.branch}

	failure := ""
	for attempt := 1; ; attempt++ {
		st.attempts = attempt
		out.Attempts = attempt

		r.Obs.Stage(StageGenerate, attempt, total)
		changes, err := r.generate(ctx, st, failure)
		if err != nil {
			return out, err
		}

		r.Obs.Stage(StageCommit, attempt, total)
		if err := r.commit(ctx, st, changes, attempt > 1); err != nil {
			return out, err
		}
		out.CommitSHA = st.head.Commit
		out.CommitURL = st.repo.HTMLURL + "/commit/" + st.head.Commit
		out.Kind = st.kind

		r.Obs.Stage(StageBuild, attempt, total)
		runs, err := r.build(ctx, st)
		if len(runs) > 0 {
			out.RunURL = runs[0].Run.HTMLURL
		}
		if err != nil {
			return out, err
		}
		if allGreen(runs) {
			r.Obs.Stage(StageDeliver, attempt, total)
			r.deliver(ctx, st, runs, &out)
			return out, nil
		}

		failure, err = r.describeFailure(ctx, st, runs)
		if err != nil {
			return out, err
		}
		if attempt >= total {
			return out, &BuildFailedError{Attempts: attempt, RunURL: out.RunURL}
		}
		r.logf(Warn, "Сборка не прошла. Отправляю журнал ошибок модели — исправление %d из %d", attempt, task.MaxFixes)
	}
}

func (r *Runner) prepare(ctx context.Context, st *state) error {
	task := st.task

	// Colibri first: it is the cheapest thing to find broken, and nothing
	// should be created on GitHub when no code can be generated.
	models, err := r.LLM.Models(ctx)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return errors.New("Колибри не сообщил ни одной модели — сервер запущен без модели?")
	}
	chosen := models[0]
	if task.Model != "" {
		found := false
		for _, m := range models {
			if m.ID == task.Model {
				chosen, found = m, true
				break
			}
		}
		if !found {
			r.logf(Warn, "Модели «%s» на сервере нет — использую «%s»", task.Model, chosen.ID)
		}
	}
	st.model, st.ctxLen = chosen.ID, chosen.ContextLength
	if st.ctxLen > 0 {
		r.logf(Info, "Колибри на связи: модель %s, контекст %d токенов", st.model, st.ctxLen)
	} else {
		r.logf(Info, "Колибри на связи: модель %s", st.model)
	}

	user, err := r.GH.CurrentUser(ctx)
	if err != nil {
		return err
	}
	st.login = user.Login
	r.logf(Info, "GitHub: вошли как %s", user.Login)
	if user.Classic {
		if !user.HasScope("repo") && !user.HasScope("public_repo") {
			r.logf(Warn, "У токена нет области repo — запись в репозиторий, скорее всего, будет отклонена")
		}
		if !user.HasScope("workflow") {
			r.logf(Warn, "У токена нет области workflow — GitHub не даст записать файл сборки")
		}
	}

	repo, err := r.GH.GetRepo(ctx, task.Owner, task.Repo)
	created := false
	if ghapi.IsNotFound(err) {
		visibility := "публичный"
		if task.Private {
			visibility = "приватный"
		}
		r.logf(Info, "Репозитория %s/%s нет — создаю (%s)", task.Owner, task.Repo, visibility)
		repo, err = r.GH.CreateRepo(ctx, user.Login, task.Owner, task.Repo, "Создано Kolibri Forge", task.Private)
		if err != nil {
			return fmt.Errorf("не удалось создать репозиторий: %w", err)
		}
		created = true
	} else if err != nil {
		return err
	}
	if repo.Archived {
		return fmt.Errorf("репозиторий %s в архиве — в него нельзя записывать", repo.FullName)
	}
	st.repo = repo
	st.branch = strings.TrimSpace(task.Branch)
	if st.branch == "" {
		st.branch = repo.DefaultBranch
	}
	if st.branch == "" {
		st.branch = "main"
	}

	head, err := r.branchHead(ctx, st, created)
	if err != nil {
		return err
	}
	st.head = head

	entries, truncated, err := r.GH.Tree(ctx, task.Owner, task.Repo, head.Tree)
	if err != nil {
		return err
	}
	if truncated {
		r.logf(Warn, "Репозиторий очень большой: GitHub показал не все файлы")
	}
	blobs := 0
	for _, e := range entries {
		if e.Type == "blob" {
			st.modes[e.Path] = e.Mode
			st.opaque[e.Path] = true
			blobs++
		}
	}
	if blobs > 0 {
		r.loadSources(ctx, st)
	}
	if blobs > 0 && !created {
		r.logf(Info, "В ветке %s уже %d %s — модель получит их как контекст", st.branch, blobs, plural(blobs, "файл", "файла", "файлов"))
	}
	return nil
}

// branchHead finds the tip of the working branch, creating the branch (or
// the very first commit) when needed.
func (r *Runner) branchHead(ctx context.Context, st *state, justCreated bool) (ghapi.Head, error) {
	owner, name := st.task.Owner, st.task.Repo
	var head ghapi.Head
	var err error
	// A repository created a moment ago may not have its first commit yet.
	for try := 0; ; try++ {
		head, err = r.GH.BranchHead(ctx, owner, name, st.branch)
		if err == nil {
			return head, nil
		}
		if !justCreated || try >= 5 || !(ghapi.IsNotFound(err) || ghapi.IsEmptyRepo(err)) {
			break
		}
		if !sleep(ctx, time.Second) {
			return head, ctx.Err()
		}
	}

	if ghapi.IsEmptyRepo(err) {
		r.logf(Info, "Репозиторий пуст — делаю первый коммит")
		if err := r.GH.InitEmptyRepo(ctx, owner, name, st.branch, name); err != nil {
			return head, fmt.Errorf("не удалось сделать первый коммит: %w", err)
		}
		return r.GH.BranchHead(ctx, owner, name, st.branch)
	}
	if ghapi.IsNotFound(err) && st.repo.DefaultBranch != "" && st.branch != st.repo.DefaultBranch {
		base, baseErr := r.GH.BranchHead(ctx, owner, name, st.repo.DefaultBranch)
		if ghapi.IsEmptyRepo(baseErr) {
			r.logf(Info, "Репозиторий пуст — делаю первый коммит в ветке %s", st.branch)
			if err := r.GH.InitEmptyRepo(ctx, owner, name, st.branch, name); err != nil {
				return head, fmt.Errorf("не удалось сделать первый коммит: %w", err)
			}
			return r.GH.BranchHead(ctx, owner, name, st.branch)
		}
		if baseErr != nil {
			return head, baseErr
		}
		r.logf(Info, "Ветки %s нет — создаю её от %s", st.branch, st.repo.DefaultBranch)
		if err := r.GH.CreateBranch(ctx, owner, name, st.branch, base.Commit); err != nil {
			return head, fmt.Errorf("не удалось создать ветку %s: %w", st.branch, err)
		}
		return base, nil
	}
	if ghapi.IsNotFound(err) {
		return head, fmt.Errorf("ветка %s не найдена в %s/%s", st.branch, owner, name)
	}
	return head, err
}

const (
	maxArchive    = 64 << 20
	maxSourceFile = 256 << 10
	maxSourceAll  = 8 << 20
)

// loadSources reads the text files of the branch so the model can see the
// project it is changing. Failure is not fatal: the model then only gets
// the file names.
func (r *Runner) loadSources(ctx context.Context, st *state) {
	var buf bytes.Buffer
	if err := r.GH.Archive(ctx, st.task.Owner, st.task.Repo, st.head.Commit, &buf, maxArchive); err != nil {
		if ctx.Err() == nil {
			r.logf(Warn, "Не удалось скачать исходники ветки: %v. Модель увидит только имена файлов", err)
		}
		return
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		r.logf(Warn, "Архив исходников не читается: %v", err)
		return
	}
	total := 0
	for _, f := range zr.File {
		// Entries look like "owner-repo-sha/path/in/repo".
		_, rel, ok := strings.Cut(f.Name, "/")
		if !ok || rel == "" || f.FileInfo().IsDir() || !st.opaque[rel] {
			continue
		}
		if f.UncompressedSize64 > maxSourceFile || total+int(f.UncompressedSize64) > maxSourceAll {
			continue
		}
		data, err := readZipFile(f, maxSourceFile)
		if err != nil || !isText(data) {
			continue
		}
		total += len(data)
		st.project[rel] = string(data)
		delete(st.opaque, rel)
	}
}

func isText(data []byte) bool {
	return utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}

// promptKind is the stack the model is told to follow.
func (st *state) promptKind() workflow.Kind {
	if st.task.Kind != "" && st.task.Kind != workflow.Auto {
		return st.task.Kind
	}
	if st.kind != "" {
		return st.kind // decided by an earlier attempt
	}
	if k := st.detect(); k != workflow.Generic {
		return k
	}
	return workflow.Auto
}

func (st *state) detect() workflow.Kind {
	paths := make([]string, 0, len(st.project)+len(st.opaque))
	for p := range st.project {
		paths = append(paths, p)
	}
	for p := range st.opaque {
		paths = append(paths, p)
	}
	return workflow.Detect(paths, func(p string) string { return st.project[p] })
}

func (st *state) opaquePaths() []string {
	out := make([]string, 0, len(st.opaque))
	for p := range st.opaque {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// generate asks the model for code and turns the reply into commit changes.
// failure is the build log of the previous attempt, "" on the first one.
func (r *Runner) generate(ctx context.Context, st *state, failure string) ([]ghapi.Change, error) {
	task := st.task
	kind := st.promptKind()
	fixed := len(systemPrompt) + len(task.Prompt) + len(failure) + 1500
	budget := fileBudget(st.ctxLen, task.MaxTokens, fixed)

	var reply string
	var cutOff bool
	for shrink := 0; ; shrink++ {
		var messages []colibri.Message
		title := "Задача"
		if failure == "" {
			messages = taskMessages(task, kind, st.project, st.opaquePaths(), budget)
		} else {
			title = fmt.Sprintf("Исправление сборки (попытка %d)", st.attempts)
			messages = fixMessages(task, kind, st.project, st.opaquePaths(), budget, failure)
		}
		r.Obs.ModelStart(title)
		r.logf(Info, "Отправляю запрос модели %s (%s)", st.model, sizeText(messagesLen(messages)))

		var err error
		reply, cutOff, err = r.ask(ctx, st, messages)
		if err == nil {
			break
		}
		if colibri.ContextExceeded(err) && shrink < 2 && budget > 0 {
			budget /= 2
			if shrink == 1 {
				budget = 0
			}
			r.logf(Warn, "Запрос не поместился в контекст модели — сокращаю исходники и повторяю")
			continue
		}
		return nil, err
	}

	parsed, perr := codeparse.Parse(reply, cutOff)
	if errors.Is(perr, codeparse.ErrNoFiles) && !parsed.Truncated && ctx.Err() == nil {
		r.logf(Warn, "В ответе нет файлов в нужном формате — прошу модель оформить ответ заново")
		messages := []colibri.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: "Task:\n" + strings.TrimSpace(task.Prompt) + "\n\n" + buildRules(kind, task.Targets)},
		}
		if prev := strings.TrimSpace(clip(reply, 6000)); prev != "" {
			messages = append(messages, colibri.Message{Role: "assistant", Content: prev})
		}
		messages = append(messages, colibri.Message{Role: "user", Content: reformatRequest})
		r.Obs.ModelStart("Повтор: ответ в нужном формате")
		var err error
		reply, cutOff, err = r.ask(ctx, st, messages)
		if err != nil {
			return nil, err
		}
		parsed, perr = codeparse.Parse(reply, cutOff)
	}
	if perr != nil {
		if parsed.Truncated {
			return nil, fmt.Errorf("ответ модели оборвался на первом же файле (%s): не хватило лимита токенов. Увеличьте «Макс. токенов ответа» или контекст сервера", parsed.TruncatedPath)
		}
		return nil, errors.New("модель не вернула файлы в нужном формате. Попробуйте уточнить задачу или выбрать другую модель")
	}
	if parsed.Truncated {
		r.logf(Warn, "Ответ оборвался на файле %s — он пропущен. Если сборка упадёт, модель допишет его при исправлении", parsed.TruncatedPath)
	}
	for _, p := range parsed.Rejected {
		r.logf(Warn, "Пропущен недопустимый путь из ответа: %s", clip(p, 80))
	}
	if parsed.Summary != "" {
		st.summary = parsed.Summary
	} else if failure == "" {
		st.summary = ""
	}

	// Apply the reply to the in-memory project.
	var changes []ghapi.Change
	var view []FileChange
	for _, f := range parsed.Files {
		if strings.HasPrefix(f.Path, ".github/") {
			r.logf(Warn, "Модель прислала %s — файлы в .github/ ведёт приложение, пропускаю", f.Path)
			continue
		}
		old, existed := st.project[f.Path]
		existed = existed || st.opaque[f.Path]
		if f.Delete {
			if !existed {
				continue
			}
			delete(st.project, f.Path)
			delete(st.opaque, f.Path)
			changes = append(changes, ghapi.Change{Path: f.Path, Delete: true, Mode: st.modes[f.Path]})
			view = append(view, FileChange{Path: f.Path, Status: "deleted"})
			continue
		}
		if existed && old == f.Content && !st.opaque[f.Path] {
			continue // the model repeated a file without changing it
		}
		status := "new"
		if existed {
			status = "changed"
		}
		st.project[f.Path] = f.Content
		delete(st.opaque, f.Path)
		changes = append(changes, ghapi.Change{Path: f.Path, Content: []byte(f.Content), Mode: fileMode(st.modes[f.Path], f.Content)})
		view = append(view, FileChange{Path: f.Path, Content: f.Content, Status: status})
	}
	if len(changes) == 0 {
		if failure != "" {
			return nil, errors.New("модель не изменила ни одного файла — исправить сборку не получилось")
		}
		return nil, errors.New("модель не предложила изменений: в ответе нет новых или изменённых файлов")
	}

	// Decide the stack and make sure the matching workflow is in place.
	if task.Kind != "" && task.Kind != workflow.Auto {
		st.kind = task.Kind
	} else {
		st.kind = st.detect()
		if info, ok := workflow.Lookup(st.kind); ok {
			if st.kind == workflow.Generic {
				r.logf(Warn, "Тип проекта не распознан — GitHub соберёт только архив исходников. Выберите тип проекта вручную, чтобы получить программу")
			} else if st.attempts == 1 {
				r.logf(Info, "Тип проекта: %s", info.Title)
			}
		}
	}
	wf, err := workflow.Render(workflow.Options{
		Kind: st.kind, Branch: st.branch, AppName: task.Repo, Targets: task.Targets, Release: task.Release,
	})
	if err != nil {
		return nil, err
	}
	if st.project[workflow.Path] != wf {
		status := "new"
		if _, ok := st.project[workflow.Path]; ok || st.opaque[workflow.Path] {
			status = "changed"
		}
		st.project[workflow.Path] = wf
		delete(st.opaque, workflow.Path)
		changes = append(changes, ghapi.Change{Path: workflow.Path, Content: []byte(wf)})
		view = append(view, FileChange{Path: workflow.Path, Content: wf, Status: status})
	}

	sort.SliceStable(view, func(i, j int) bool { return view[i].Path < view[j].Path })
	r.Obs.Files(view)
	return changes, nil
}

// ask streams one reply, continuing it when the token limit cuts it short.
func (r *Runner) ask(ctx context.Context, st *state, messages []colibri.Message) (reply string, cutOff bool, err error) {
	req := colibri.ChatRequest{Model: st.model, Messages: messages, Temperature: st.task.Temperature, MaxTokens: st.task.MaxTokens}
	onDelta := func(d colibri.Delta) {
		if d.Reasoning != "" {
			r.Obs.ModelDelta(d.Reasoning, true)
		}
		if d.Content != "" {
			r.Obs.ModelDelta(d.Content, false)
		}
	}
	started := time.Now()
	res, err := r.LLM.ChatStream(ctx, req, onDelta)
	if err != nil {
		return "", false, err
	}
	reply = res.Content
	tokens := res.Usage.CompletionTokens

	// Colibri continues a turn when the conversation ends with a partial
	// assistant message, so a reply cut by max_tokens can be resumed.
	for part := 0; res.FinishReason == "length" && part < 3; part++ {
		partial := strings.TrimRight(reply, " \t\r\n")
		if partial == "" {
			break
		}
		r.logf(Warn, "Ответ упёрся в лимит токенов — прошу модель продолжить")
		cont := append(append([]colibri.Message{}, messages...), colibri.Message{Role: "assistant", Content: partial})
		req.Messages = cont
		next, err := r.LLM.ChatStream(ctx, req, onDelta)
		if err != nil {
			if ctx.Err() != nil {
				return "", false, ctx.Err()
			}
			// No room left in the context: work with what arrived.
			r.logf(Warn, "Продолжить ответ не удалось: %v", err)
			return reply, true, nil
		}
		reply = partial + next.Content
		tokens += next.Usage.CompletionTokens
		res = next
	}

	elapsed := time.Since(started)
	if tokens > 0 && elapsed > time.Second {
		r.logf(Info, "Ответ получен: %s, %d токенов за %s (%.1f ток/с)", sizeText(len(reply)), tokens, durationText(elapsed), float64(tokens)/elapsed.Seconds())
	} else {
		r.logf(Info, "Ответ получен: %s за %s", sizeText(len(reply)), durationText(elapsed))
	}
	return reply, res.FinishReason == "length", nil
}

func messagesLen(messages []colibri.Message) int {
	n := 0
	for _, m := range messages {
		n += len(m.Content)
	}
	return n
}

// fileMode keeps the mode a file already has and marks new scripts executable.
func fileMode(existing, content string) string {
	if existing == "100755" || existing == "100644" {
		return existing
	}
	if strings.HasPrefix(content, "#!") {
		return "100755"
	}
	return "100644"
}

func (r *Runner) commit(ctx context.Context, st *state, changes []ghapi.Change, isFix bool) error {
	subject := st.summary
	if subject == "" {
		subject = clip(firstLine(st.task.Prompt), 72)
	}
	if isFix && !strings.HasPrefix(strings.ToLower(subject), "fix") {
		subject = "fix: " + subject
	}
	message := subject + "\n\n" + "Задача: " + clip(strings.TrimSpace(st.task.Prompt), 500) +
		"\n\nСгенерировано моделью " + st.model + " через Kolibri Forge."

	head, err := r.GH.Commit(ctx, st.task.Owner, st.task.Repo, st.branch, st.head, message, changes)
	if err != nil {
		return err
	}
	st.head = head
	for _, ch := range changes {
		if ch.Delete {
			delete(st.modes, ch.Path)
		} else {
			st.modes[ch.Path] = fileMode(ch.Mode, "")
		}
	}
	short := head.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	r.logf(Good, "Коммит %s в %s/%s (%s): %d %s — %s", short, st.task.Owner, st.task.Repo, st.branch,
		len(changes), plural(len(changes), "файл", "файла", "файлов"), subject)
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
