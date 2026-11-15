package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/workflow"
)

func allGreen(runs []RunStatus) bool {
	if len(runs) == 0 {
		return false
	}
	for _, r := range runs {
		if !r.Run.OK() {
			return false
		}
	}
	return true
}

// build waits for the GitHub Actions runs of the current commit to finish.
func (r *Runner) build(ctx context.Context, st *state) ([]RunStatus, error) {
	owner, name, sha := st.task.Owner, st.task.Repo, st.head.Commit
	r.logf(Info, "Жду, когда GitHub Actions начнёт сборку…")

	started := time.Now()
	announced := map[int64]bool{}
	finished := map[int64]bool{}
	failures := 0
	var last []RunStatus

	for {
		runs, err := r.GH.RunsForCommit(ctx, owner, name, sha)
		var statuses []RunStatus
		if err == nil {
			sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })
			for _, run := range runs {
				var jobs []ghapi.Job
				jobs, err = r.GH.Jobs(ctx, owner, name, run.ID)
				if err != nil {
					break
				}
				statuses = append(statuses, RunStatus{Run: run, Jobs: jobs})
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return last, ctx.Err()
			}
			// The network may blink during a long build; GitHub keeps building.
			failures++
			if failures == 1 {
				r.logf(Warn, "Не удалось узнать состояние сборки (%v) — повторяю", err)
			}
			if failures >= 10 {
				return last, fmt.Errorf("состояние сборки недоступно: %w", err)
			}
		} else {
			failures = 0
			last = statuses
			r.Obs.Build(statuses)

			done := len(statuses) > 0
			for _, s := range statuses {
				if !announced[s.Run.ID] {
					announced[s.Run.ID] = true
					r.logf(Info, "Сборка №%d «%s» запущена: %s", s.Run.RunNumber, s.Run.Name, s.Run.HTMLURL)
				}
				if !s.Run.Done() {
					done = false
					continue
				}
				if !finished[s.Run.ID] {
					finished[s.Run.ID] = true
					if s.Run.OK() {
						r.logf(Good, "Сборка №%d прошла за %s", s.Run.RunNumber, durationText(time.Since(started)))
					} else {
						r.logf(Bad, "Сборка №%d завершилась неудачно (%s)", s.Run.RunNumber, conclusionText(s.Run.Conclusion))
					}
				}
			}
			if done {
				return statuses, nil
			}
			if len(statuses) == 0 && time.Since(started) > r.runStartTimeout() {
				return nil, fmt.Errorf("GitHub не начал сборку за %s. Проверьте, что Actions включены: %s/settings/actions",
					durationText(r.runStartTimeout()), st.repo.HTMLURL)
			}
		}
		if time.Since(started) > r.buildTimeout() {
			return last, fmt.Errorf("сборка идёт дольше %s — прекращаю ожидание. Она продолжается на GitHub", durationText(r.buildTimeout()))
		}
		if !sleep(ctx, r.pollInterval()) {
			return last, ctx.Err()
		}
	}
}

func conclusionText(c string) string {
	switch c {
	case "failure":
		return "ошибка"
	case "cancelled":
		return "отменена"
	case "timed_out":
		return "превышено время"
	case "startup_failure":
		return "не запустилась"
	case "action_required":
		return "требуется подтверждение на GitHub"
	}
	return c
}

const (
	maxFailureJobs  = 2
	maxFailureChars = 6000
)

// describeFailure collects what went wrong in a form the model can act on.
// An error means the failure is not something a code change can fix.
func (r *Runner) describeFailure(ctx context.Context, st *state, runs []RunStatus) (string, error) {
	owner, name := st.task.Owner, st.task.Repo
	var failed []ghapi.Job
	cancelled := false
	for _, s := range runs {
		if s.Run.OK() {
			continue
		}
		if s.Run.Conclusion == "cancelled" {
			cancelled = true
		}
		for _, j := range s.Jobs {
			if j.Failed() {
				failed = append(failed, j)
			}
		}
	}
	if len(failed) == 0 {
		if cancelled {
			return "", errors.New("сборку отменили на GitHub")
		}
		return "", errors.New("GitHub не смог запустить сборку: ни одно задание не выполнялось. Откройте запуск на GitHub, чтобы увидеть причину")
	}

	var b strings.Builder
	errorLine := "" // the first telling line, for the work log
	defer func() { st.lastError = errorLine }()
	seen := map[string]bool{}
	budget := maxFailureChars
	if st.ctxLen > 0 && st.ctxLen < 20000 {
		budget = 3500 // leave room for the sources in a small context
	}
	per := budget / min(len(failed), maxFailureJobs)
	used := 0
	for _, job := range failed {
		if used == maxFailureJobs {
			fmt.Fprintf(&b, "(%d more failed jobs are not shown)\n", len(failed)-used)
			break
		}
		excerpt := ""
		log, err := r.GH.JobLog(ctx, owner, name, job.ID)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// Logs can be unavailable (expired, permissions): annotations
			// still carry the compiler's messages.
			excerpt = r.annotationText(ctx, st, job.ID)
			if excerpt == "" {
				r.logf(Warn, "Журнал задания «%s» недоступен: %v", job.Name, err)
			}
		} else {
			excerpt = ExtractFailure(log, per)
		}
		if excerpt == "" || seen[excerpt] {
			continue
		}
		seen[excerpt] = true
		used++
		step := job.FailedStep()
		if step != "" {
			fmt.Fprintf(&b, "Job %q failed at step %q:\n", job.Name, step)
			r.logf(Bad, "Задание «%s» упало на шаге «%s»", job.Name, step)
		} else {
			fmt.Fprintf(&b, "Job %q failed:\n", job.Name)
			r.logf(Bad, "Задание «%s» упало", job.Name)
		}
		b.WriteString(excerpt)
		b.WriteString("\n\n")
		if line := lastErrorLine(excerpt); line != "" {
			r.logf(Bad, "%s", clip(line, 300))
			if errorLine == "" {
				errorLine = line
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", errors.New("сборка упала, но журнал ошибок получить не удалось — модель не сможет её исправить. Откройте запуск на GitHub")
	}
	return text, nil
}

func (r *Runner) annotationText(ctx context.Context, st *state, jobID int64) string {
	anns, err := r.GH.Annotations(ctx, st.task.Owner, st.task.Repo, jobID)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, a := range anns {
		if a.Level != "failure" {
			continue
		}
		if a.Path != "" && a.Path != ".github" {
			fmt.Fprintf(&b, "%s:%d: ", a.Path, a.StartLine)
		}
		b.WriteString(strings.TrimSpace(a.Message))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// lastErrorLine picks the line of an excerpt most worth showing in the journal.
func lastErrorLine(excerpt string) string {
	lines := strings.Split(excerpt, "\n")
	best := ""
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "$ ") || strings.HasPrefix(t, "ERROR: Process completed") {
			continue
		}
		low := strings.ToLower(t)
		if strings.Contains(low, "error") || strings.Contains(low, "undefined") || strings.Contains(low, "cannot") || strings.Contains(low, "fail") {
			if best == "" {
				best = t
			}
		}
	}
	if best == "" {
		for i := len(lines) - 1; i >= 0; i-- {
			if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "ERROR: Process completed") {
				return t
			}
		}
	}
	return best
}

const maxArtifact = 2 << 30

// deliver downloads the artifacts of a green build and finds its release.
// Problems here do not fail the run: the build exists on GitHub either way.
func (r *Runner) deliver(ctx context.Context, st *state, runs []RunStatus, out *Outcome) {
	owner, name := st.task.Owner, st.task.Repo
	// The run produced by the managed workflow is the one with the product.
	main := runs[0]
	for _, s := range runs {
		if s.Run.Name == "Kolibri Build" {
			main = s
		}
	}
	out.RunURL = main.Run.HTMLURL

	if st.task.Release {
		tag := fmt.Sprintf("build-%d", main.Run.RunNumber)
		if rel, err := r.GH.ReleaseByTag(ctx, owner, name, tag); err == nil {
			out.ReleaseURL = rel.HTMLURL
			r.logf(Good, "Релиз опубликован: %s", rel.HTMLURL)
		} else if !ghapi.IsNotFound(err) && ctx.Err() == nil {
			r.logf(Warn, "Не удалось проверить релиз: %v", err)
		}
	}

	if strings.TrimSpace(st.task.OutputDir) == "" {
		return
	}
	artifacts, err := r.GH.Artifacts(ctx, owner, name, main.Run.ID)
	if err != nil {
		r.logf(Warn, "Не удалось получить список файлов сборки: %v", err)
		return
	}
	if len(artifacts) == 0 {
		r.logf(Warn, "Сборка прошла, но не оставила файлов для скачивания")
		return
	}
	dir := filepath.Join(st.task.OutputDir, workflow.SafeName(name), fmt.Sprintf("build-%d", main.Run.RunNumber))
	for _, a := range artifacts {
		if a.Expired {
			continue
		}
		target := filepath.Join(dir, workflow.SafeName(a.Name))
		r.logf(Info, "Скачиваю %s (%s)…", a.Name, sizeText(int(a.Size)))
		if err := r.fetchArtifact(ctx, st, a, target); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.logf(Warn, "Не удалось скачать %s: %v. Файл доступен на странице сборки", a.Name, err)
			continue
		}
		out.Artifacts = append(out.Artifacts, a.Name)
	}
	if len(out.Artifacts) > 0 {
		out.Dir = dir
		r.logf(Good, "Готово: сборка лежит в %s", dir)
	}
}

func (r *Runner) fetchArtifact(ctx context.Context, st *state, a ghapi.Artifact, target string) error {
	tmp, err := os.CreateTemp("", "kolibri-artifact-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := r.GH.DownloadArtifact(ctx, st.task.Owner, st.task.Repo, a.ID, tmp, maxArtifact); err != nil {
		return err
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return fmt.Errorf("архив повреждён: %w", err)
	}
	return unzip(zr, target)
}

// unzip extracts an archive into dir, refusing entries that would land
// outside of it.
func unzip(zr *zip.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		rel := filepath.FromSlash(f.Name)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("в архиве недопустимый путь %q", f.Name)
		}
		dest := filepath.Join(root, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := extractFile(f, dest); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(f *zip.File, dest string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	var magic [4]byte
	n, _ := io.ReadFull(src, magic[:])
	if _, err := dst.Write(magic[:n]); err != nil {
		dst.Close()
		return err
	}
	if _, err := io.Copy(dst, io.LimitReader(src, maxArtifact)); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	// GitHub artifacts drop Unix permissions; give programs and scripts
	// their executable bit back so the build runs right after download.
	if runtime.GOOS != "windows" && isExecutable(magic[:n]) {
		return os.Chmod(dest, 0o755)
	}
	return nil
}

func isExecutable(magic []byte) bool {
	switch {
	case bytes.HasPrefix(magic, []byte("\x7fELF")),
		bytes.HasPrefix(magic, []byte{0xcf, 0xfa, 0xed, 0xfe}),
		bytes.HasPrefix(magic, []byte{0xca, 0xfe, 0xba, 0xbe}),
		bytes.HasPrefix(magic, []byte("#!")):
		return true
	}
	return false
}

func readZipFile(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}

// plural picks the Russian plural form for n: 1 файл, 2 файла, 5 файлов.
func plural(n int, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	switch {
	case n%100 >= 11 && n%100 <= 14:
		return many
	case n%10 == 1:
		return one
	case n%10 >= 2 && n%10 <= 4:
		return few
	}
	return many
}

func sizeText(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d Б", n)
	case n < 1000*1000:
		return fmt.Sprintf("%.1f КБ", float64(n)/1000)
	}
	return fmt.Sprintf("%.1f МБ", float64(n)/1e6)
}

func durationText(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d мин %02d с", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d ч %02d мин", int(d.Hours()), int(d.Minutes())%60)
}
