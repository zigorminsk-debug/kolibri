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
	"github.com/zigorminsk-debug/kolibri/internal/worklog"
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
	// Next is the model's note on what is left to do, "" when it considers
	// the task finished.
	Next string
	// LogURL is the work log in the repository, "" before the first commit.
	LogURL string
}

// InterruptedError means the model's reply broke off (the server stopped,
// the connection dropped, the model fell silent) after some files had
// already arrived. Those files are committed; a new run continues from them.
type InterruptedError struct {
	Err   error
	Saved int
}

func (e *InterruptedError) Error() string {
	return fmt.Sprintf("%v. Полученное сохранено в репозитории (%d %s) и записано в журнал работы — запустите задачу снова, работа продолжится с этого места",
		e.Err, e.Saved, plural(e.Saved, "файл", "файла", "файлов"))
}

func (e *InterruptedError) Unwrap() error { return e.Err }

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

	// worklog is the project's work log (worklog.Path), kept apart from
	// the project files; logged is true once this run wrote to it.
	worklog string
	logged  bool
	// pending is true while commits made without a build wait for one.
	pending bool
	// next is the model's latest note on what remains.
	next string
	// lastError is the most telling line of the latest build failure.
	lastError string
}

// maxParts bounds how many replies in a row one generation may take when
// each of them is cut short.
const maxParts = 6

// generation is what one model reply turned into.
type generation struct {
	changes []ghapi.Change
	files   []string // "+ path", "~ path", "- path" — for the work log
	// partial says why the reply is incomplete, "" when it is whole.
	partial string
	// resume tells the model how to go on in the next request.
	resume string
	// broken is the stream error that cut the reply, when there was one.
	broken error
}

// commitKind says what a commit is for.
type commitKind struct {
	fix     bool // a reply to a failed build
	resumed bool // a reply that continues an unfinished one
	partial bool // an unfinished reply: saved, but not built
}

// Run executes a task. It returns when the build is green and downloaded,
// when it gives up, or when ctx is cancelled.
func (r *Runner) Run(ctx context.Context, task Task) (out Outcome, err error) {
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
	out = Outcome{RepoURL: st.repo.HTMLURL, Branch: st.branch}
	st.takeLog()
	if n := worklog.Entries(st.worklog); n > 0 {
		r.logf(Info, "В репозитории есть журнал работы (%d %s) — модель продолжит по нему", n, plural(n, "запись", "записи", "записей"))
	}
	// However the run ends, the log in the repository says how.
	defer func() { r.closeLog(ctx, st, &out, err) }()

	failure := ""
	for attempt := 1; ; attempt++ {
		st.attempts = attempt
		out.Attempts = attempt

		// One generation is usually one reply. A reply that is cut short
		// is saved without a build and continued in a fresh request, so a
		// project larger than the model's reply still gets written.
		var gen generation
		resume := ""
		for part := 1; ; part++ {
			r.Obs.Stage(StageGenerate, attempt, total)
			gen, err = r.generate(ctx, st, failure, resume)
			if err != nil {
				return out, err
			}
			if gen.partial == "" {
				break
			}
			r.Obs.Stage(StageCommit, attempt, total)
			r.logf(Warn, "Ответ неполный: %s. Сохраняю готовые файлы в репозиторий без запуска сборки", gen.partial)
			if err := r.commit(ctx, st, gen, commitKind{fix: attempt > 1, resumed: resume != "", partial: true}); err != nil {
				return out, err
			}
			out.CommitSHA = st.head.Commit
			out.CommitURL = st.repo.HTMLURL + "/commit/" + st.head.Commit
			out.LogURL = st.logURL()
			if gen.broken != nil {
				return out, &InterruptedError{Err: gen.broken, Saved: len(gen.changes)}
			}
			if part >= maxParts {
				return out, fmt.Errorf("задача не уложилась в %d ответов подряд: каждый обрывался. Сделанное сохранено в репозитории и записано в журнал работы — запустите задачу снова, чтобы продолжить, или увеличьте «Макс. токенов ответа»", maxParts)
			}
			r.logf(Info, "Продолжаю с чистым контекстом: модель получит сохранённые файлы и журнал работы (часть %d)", part+1)
			resume = gen.resume
		}

		r.Obs.Stage(StageCommit, attempt, total)
		if err := r.commit(ctx, st, gen, commitKind{fix: attempt > 1, resumed: resume != ""}); err != nil {
			return out, err
		}
		out.CommitSHA = st.head.Commit
		out.CommitURL = st.repo.HTMLURL + "/commit/" + st.head.Commit
		out.Kind = st.kind
		out.Next = st.next
		out.LogURL = st.logURL()

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
			if st.next != "" {
				r.logf(Warn, "Модель считает задачу незаконченной. Осталось: %s. Запустите задачу снова — модель продолжит по журналу работы",
					clip(strings.Join(strings.Fields(st.next), " "), 400))
			}
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

// takeLog moves the work log out of the project files: the model sees it
// as history, not as a file it may rewrite.
func (st *state) takeLog() {
	st.worklog = st.project[worklog.Path]
	delete(st.project, worklog.Path)
	delete(st.opaque, worklog.Path)
}

func (st *state) logURL() string {
	if !st.logged {
		return ""
	}
	return st.repo.HTMLURL + "/blob/" + st.branch + "/" + worklog.Path
}

// closeLog writes how the run ended into the work log. The commit is marked
// [skip ci]: it changes no code and must not start another build.
func (r *Runner) closeLog(ctx context.Context, st *state, out *Outcome, runErr error) {
	if !st.logged {
		return
	}
	var failed *BuildFailedError
	var cut *InterruptedError
	var note string
	switch {
	case runErr == nil:
		note = "сборка прошла"
		if out.RunURL != "" {
			note += " — " + out.RunURL
		}
		if out.ReleaseURL != "" {
			note += "; релиз " + out.ReleaseURL
		}
	case errors.As(runErr, &failed):
		note = failed.Error()
		if st.lastError != "" {
			note += ". Последняя ошибка: " + clip(st.lastError, 300)
		}
	case errors.Is(runErr, context.Canceled):
		note = "работа остановлена пользователем"
	case errors.As(runErr, &cut):
		note = "ответ модели прервался: " + clip(cut.Err.Error(), 300)
	default:
		note = "работа прервана: " + clip(runErr.Error(), 400)
	}

	// A cancelled run still gets its line: use a context of its own.
	if ctx.Err() != nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	doc := worklog.Note(st.worklog, note)
	head, err := r.GH.Commit(ctx, st.task.Owner, st.task.Repo, st.branch, st.head,
		"docs: итог в журнале работы\n\n"+note+"\n\n[skip ci]",
		[]ghapi.Change{{Path: worklog.Path, Content: []byte(doc), Mode: "100644"}})
	if err != nil {
		r.logf(Warn, "Не удалось записать итог в журнал работы: %v", err)
		return
	}
	st.worklog, st.head = doc, head
	r.logf(Info, "Итог записан в журнал работы: %s", st.logURL())
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
// generate asks the model for code and applies the reply to the in-memory
// project. failure is the build log to fix, resume the note on continuing
// a reply that was cut short; both may be empty.
func (r *Runner) generate(ctx context.Context, st *state, failure, resume string) (generation, error) {
	var none generation
	task := st.task
	kind := st.promptKind()
	// The log gets a small, fixed share of the context: enough for the
	// latest steps and the plan.
	history := worklog.Tail(st.worklog, logShare(st.ctxLen))
	fixed := len(systemPrompt) + len(task.Prompt) + len(failure) + len(history) + len(resume) + 1500
	budget := fileBudget(st.ctxLen, task.MaxTokens, fixed)

	var reply string
	var cutOff bool
	var broken error
	for shrink := 0; ; shrink++ {
		var messages []colibri.Message
		title := "Задача"
		extra := promptExtra{history: history, resume: resume}
		if failure == "" {
			messages = taskMessages(task, kind, st.project, st.opaquePaths(), budget, extra)
		} else {
			title = fmt.Sprintf("Исправление сборки (попытка %d)", st.attempts)
			messages = fixMessages(task, kind, st.project, st.opaquePaths(), budget, failure, extra)
		}
		if resume != "" {
			title += " — продолжение"
		}
		r.Obs.ModelStart(title)
		r.logf(Info, "Отправляю запрос модели %s (%s)", st.model, sizeText(messagesLen(messages)))

		var err error
		reply, cutOff, broken, err = r.ask(ctx, st, messages)
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
		return none, err
	}

	parsed, perr := codeparse.Parse(reply, cutOff)
	if errors.Is(perr, codeparse.ErrNoFiles) && !parsed.Truncated && broken == nil && ctx.Err() == nil {
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
		reply, cutOff, broken, err = r.ask(ctx, st, messages)
		if err != nil {
			return none, err
		}
		parsed, perr = codeparse.Parse(reply, cutOff)
	}
	if perr != nil {
		if broken != nil {
			// Nothing whole arrived before the stream broke.
			return none, broken
		}
		if parsed.Truncated {
			return none, fmt.Errorf("ответ модели оборвался на первом же файле (%s): не хватило лимита токенов. Увеличьте «Макс. токенов ответа» или контекст сервера", parsed.TruncatedPath)
		}
		return none, errors.New("модель не вернула файлы в нужном формате. Попробуйте уточнить задачу или выбрать другую модель")
	}

	gen := generation{broken: broken}
	if cutOff {
		// The reply did not end by itself: what is whole gets saved, the
		// rest is asked for again.
		if parsed.Truncated {
			gen.partial = "ответ оборвался на файле " + parsed.TruncatedPath
			gen.resume = "Your previous reply was cut off in the middle of the file " + parsed.TruncatedPath +
				". That file was not saved; every file completed before it is committed and shown below. " +
				"Do not repeat finished files: send " + parsed.TruncatedPath + " in full and then everything else that is still missing."
		} else {
			gen.partial = "ответ оборвался, не дойдя до конца"
			gen.resume = "Your previous reply was cut off before it ended. Every file completed in it is committed and shown below. " +
				"Do not repeat finished files: send only what is still missing."
		}
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
		if strings.HasPrefix(f.Path, ".github/") || strings.HasPrefix(f.Path, worklog.Dir) {
			r.logf(Warn, "Модель прислала %s — файлы в .github/ и .kolibri/ ведёт приложение, пропускаю", f.Path)
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
	switch {
	case len(changes) > 0:
	case gen.broken != nil:
		return none, gen.broken
	case gen.partial != "":
		return none, errors.New("ответ модели снова оборвался, и в нём нет ни одного нового файла: дальше так не продвинуться. Увеличьте «Макс. токенов ответа» или контекст сервера Колибри")
	case st.pending:
		// Earlier parts are committed but not built: this reply adds
		// nothing, so the build runs on what is there.
		r.logf(Info, "Модель сообщила, что дописывать нечего — собираю сохранённое")
	case failure != "":
		return none, errors.New("модель не изменила ни одного файла — исправить сборку не получилось")
	default:
		return none, errors.New("модель не предложила изменений: в ответе нет новых или изменённых файлов")
	}
	st.next = parsed.Next

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
		return none, err
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
	for _, v := range view {
		if v.Path == workflow.Path {
			continue // the application's own file is not the model's work
		}
		mark := "~ "
		switch v.Status {
		case "new":
			mark = "+ "
		case "deleted":
			mark = "- "
		}
		gen.files = append(gen.files, mark+v.Path)
	}
	gen.changes = changes
	return gen, nil
}

// logShare is how many characters of the work log go into a request.
func logShare(contextTokens int) int {
	if contextTokens <= 0 {
		contextTokens = 16384
	}
	share := contextTokens / 4 // about a twelfth of the context, in characters
	return min(max(share, 1500), 8000)
}

// ask streams one reply, continuing it when the token limit cuts it short.
// cutOff is true when the reply did not end by itself. broken carries the
// stream error when the reply broke off after some text had arrived: the
// caller saves what is whole before reporting it.
func (r *Runner) ask(ctx context.Context, st *state, messages []colibri.Message) (reply string, cutOff bool, broken, err error) {
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
		if ctx.Err() != nil || strings.TrimSpace(res.Content) == "" {
			return "", false, nil, err
		}
		r.logf(Warn, "Ответ модели прервался: %v", err)
		return res.Content, true, err, nil
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
				return "", false, nil, ctx.Err()
			}
			// No room left in the context, or the stream broke: work with
			// what arrived. The rest is asked for in a fresh request.
			r.logf(Warn, "Продолжить ответ в том же контексте не удалось: %v", err)
			if colibri.ContextExceeded(err) {
				return partial + next.Content, true, nil, nil
			}
			return partial + next.Content, true, err, nil
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
	return reply, res.FinishReason == "length", nil, nil
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

// commit writes one generation to the branch together with its entry in
// the work log. A partial commit is marked [skip ci]: an unfinished project
// would only fail the build.
func (r *Runner) commit(ctx context.Context, st *state, gen generation, kind commitKind) error {
	subject := st.summary
	if subject == "" {
		subject = clip(firstLine(st.task.Prompt), 72)
	}
	switch {
	case kind.partial:
		subject = "wip: " + subject
	case kind.fix && !strings.HasPrefix(strings.ToLower(subject), "fix"):
		subject = "fix: " + subject
	}
	message := subject + "\n\n" + "Задача: " + clip(strings.TrimSpace(st.task.Prompt), 500) +
		"\n\nСгенерировано моделью " + st.model + " через Kolibri Forge."
	if kind.partial {
		message += "\n\nОтвет модели неполный (" + gen.partial + "): файлы сохранены без запуска сборки.\n\n[skip ci]"
	}

	entry := worklog.Entry{
		Time: time.Now(), Kind: "Задача", Model: st.model,
		Summary: st.summary, Files: gen.files, Unfinished: gen.partial, Next: st.next,
	}
	if !st.logged {
		// The task is written once per run; later entries belong to it.
		entry.Task = st.task.Prompt
	}
	switch {
	case kind.fix:
		entry.Kind = fmt.Sprintf("Исправление сборки (попытка %d)", st.attempts)
		entry.Reason = st.lastError
	case kind.resumed:
		entry.Kind = "Продолжение оборванного ответа"
	}
	if kind.fix && kind.resumed {
		entry.Kind += ", продолжение"
	}
	doc := worklog.Append(st.worklog, entry)
	changes := append(append([]ghapi.Change{}, gen.changes...),
		ghapi.Change{Path: worklog.Path, Content: []byte(doc), Mode: "100644"})

	head, err := r.GH.Commit(ctx, st.task.Owner, st.task.Repo, st.branch, st.head, message, changes)
	if err != nil {
		return err
	}
	st.head = head
	st.worklog, st.logged = doc, true
	st.pending = kind.partial
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
	n := len(gen.changes)
	what := fmt.Sprintf("%d %s", n, plural(n, "файл", "файла", "файлов"))
	if n == 0 {
		what = "только журнал работы"
	}
	note := ""
	if kind.partial {
		note = " (без сборки)"
	}
	r.logf(Good, "Коммит %s в %s/%s (%s)%s: %s — %s", short, st.task.Owner, st.task.Repo, st.branch, note, what, subject)
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
