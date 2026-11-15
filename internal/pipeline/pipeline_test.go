package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/ghapi"
	"github.com/zigorminsk-debug/kolibri/internal/workflow"
)

const failingLog = "2026-11-15T07:00:00.0000000Z ##[group]Run actions/checkout@v7\n" +
	"2026-11-15T07:00:00.1000000Z ##[endgroup]\n" +
	"2026-11-15T07:00:01.0000000Z ##[group]Run go build -o dist/ ./...\n" +
	"2026-11-15T07:00:01.1000000Z \x1b[36;1mgo build -o dist/ ./...\x1b[0m\n" +
	"2026-11-15T07:00:01.2000000Z shell: /usr/bin/bash -e {0}\n" +
	"2026-11-15T07:00:01.3000000Z ##[endgroup]\n" +
	"2026-11-15T07:00:05.0000000Z # demo\n" +
	"2026-11-15T07:00:05.1000000Z ./main.go:5:2: undefined: BUG\n" +
	"2026-11-15T07:00:05.2000000Z ##[error]Process completed with exit code 1.\n" +
	"2026-11-15T07:00:06.0000000Z Post job cleanup.\n"

func goodReply(body string) string {
	return "SUMMARY: Add demo program\n\n=== FILE: go.mod ===\nmodule demo\n\ngo 1.22\n=== END FILE ===\n\n" +
		"=== FILE: main.go ===\n" + body + "=== END FILE ===\n"
}

func newRunner(t *testing.T, llm *fakeLLM, gh *fakeGitHub) (*Runner, *recorder) {
	rec := &recorder{}
	return &Runner{
		LLM:          &colibri.Client{BaseURL: llm.srv.URL + "/", IdleTimeout: 5 * time.Second},
		GH:           &ghapi.Client{Token: "test-token", BaseURL: gh.srv.URL},
		Obs:          rec,
		PollInterval: time.Millisecond,
	}, rec
}

func baseTask(t *testing.T) Task {
	return Task{
		Prompt: "Сделай программу demo", Owner: "me", Repo: "demo", Kind: workflow.Auto,
		Targets: []string{"windows", "linux"}, MaxFixes: 2, Private: true, Release: true,
		OutputDir: t.TempDir(), Temperature: 0.2,
	}
}

// The whole story: a new repository, a red build, a fix from the model,
// a green build, and the program on disk.
func TestRunCreatesRepoFixesBuildAndDelivers(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.judge = func(files map[string]string) (bool, string) {
		if strings.Contains(files["main.go"], "BUG") {
			return false, failingLog
		}
		return true, ""
	}
	llm := newFakeLLM(t, func(n int, _ []colibri.Message) fakeReply {
		if n == 0 {
			return fakeReply{reasoning: "думаю", content: goodReply("package main\n\nfunc main() {\n\tBUG()\n}\n") +
				"=== FILE: .github/workflows/evil.yml ===\non: push\n=== END FILE ===\n"}
		}
		return fakeReply{content: "SUMMARY: Remove undefined call\n\n=== FILE: main.go ===\npackage main\n\nfunc main() {}\n=== END FILE ===\n"}
	})
	runner, rec := newRunner(t, llm, gh)
	task := baseTask(t)

	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v\njournal:\n%s", err, rec.journal())
	}

	// GitHub side.
	files := gh.headFiles("main")
	if got, want := sortedKeys(files), []string{".github/workflows/kolibri-build.yml", "README.md", "go.mod", "main.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files in branch = %v, want %v", got, want)
	}
	if files["main.go"] != "package main\n\nfunc main() {}\n" {
		t.Errorf("main.go = %q", files["main.go"])
	}
	wf := files[workflow.Path]
	for _, want := range []string{"go build", "target: windows", "target: linux", "gh release create", `branches: ["main"]`} {
		if !strings.Contains(wf, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	if strings.Contains(wf, "macos") {
		t.Error("workflow builds for macOS although it was not requested")
	}
	hist := gh.history("main")
	if len(hist) != 3 || !strings.HasPrefix(hist[0], "fix: Remove undefined call") || !strings.HasPrefix(hist[1], "Add demo program") {
		t.Errorf("history = %q", hist)
	}

	// The fix request must carry the compiler error, not the log noise.
	fix := llm.request(1)
	for _, want := range []string{"./main.go:5:2: undefined: BUG", "$ go build -o dist/ ./...", `Job "Build (linux)" failed at step "Build"`, "BUG()", "Сделай программу demo"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix request lacks %q:\n%s", want, fix)
		}
	}
	for _, noise := range []string{"2026-11-15T", "shell: /usr/bin/bash", "\x1b[", "Post job cleanup", "kolibri-build.yml"} {
		if strings.Contains(fix, noise) {
			t.Errorf("fix request contains noise %q", noise)
		}
	}
	// The first request tells a new project from an existing one and names the stacks.
	first := llm.request(0)
	for _, want := range []string{"Choose the stack", "The program is built for: Windows, Linux.", "=== FILE: README.md ==="} {
		if !strings.Contains(first, want) {
			t.Errorf("first request lacks %q:\n%s", want, first)
		}
	}

	// Outcome and delivery.
	if out.Attempts != 2 || out.Kind != workflow.Go {
		t.Errorf("attempts=%d kind=%s", out.Attempts, out.Kind)
	}
	if out.ReleaseURL != "https://github.test/me/demo/releases/tag/build-2" {
		t.Errorf("release url = %q", out.ReleaseURL)
	}
	if out.RunURL != "https://github.test/me/demo/actions/runs/1001" {
		t.Errorf("run url = %q", out.RunURL)
	}
	wantDir := filepath.Join(task.OutputDir, "demo", "build-2")
	if out.Dir != wantDir {
		t.Errorf("dir = %q, want %q", out.Dir, wantDir)
	}
	bin := filepath.Join(wantDir, "demo-linux", "demo")
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("downloaded binary: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Errorf("binary is not executable: %v", info.Mode())
	}
	if data, _ := os.ReadFile(filepath.Join(wantDir, "demo-linux", "docs", "readme.txt")); string(data) != "hello" {
		t.Errorf("nested artifact file = %q", data)
	}

	// Observer.
	wantStages := []string{"0:1/3", "1:1/3", "2:1/3", "3:1/3", "1:2/3", "2:2/3", "3:2/3", "4:2/3"}
	if !reflect.DeepEqual(rec.stages, wantStages) {
		t.Errorf("stages = %v", rec.stages)
	}
	if rec.think.String() != "думаю" {
		t.Errorf("reasoning stream = %q", rec.think.String())
	}
	if !strings.Contains(rec.model.String(), "func main() {}") {
		t.Error("model stream was not forwarded")
	}
	if len(rec.files) != 2 || len(rec.files[0]) != 3 || len(rec.files[1]) != 1 || rec.files[1][0].Status != "changed" {
		t.Errorf("file events = %+v", rec.files)
	}
	journal := rec.journal()
	for _, want := range []string{"Репозитория me/demo нет — создаю (приватный)", ".github/workflows/evil.yml", "undefined: BUG", "Релиз опубликован"} {
		if !strings.Contains(journal, want) {
			t.Errorf("journal lacks %q:\n%s", want, journal)
		}
	}
	if strings.Contains(journal, "test-token") {
		t.Error("the token leaked into the journal")
	}
}

func TestRunOnExistingProject(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.seed(map[string]string{
		"go.mod":       "module demo\n\ngo 1.22\n",
		"main.go":      "package main\n\nfunc main() { println(\"v1\") }\n",
		"go.sum":       "example.com/x v1.0.0 h1:abc\n",
		"logo.png":     "\x89PNG\x00\x00binary",
		"old/notes.md": "old\n",
	})
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply {
		return fakeReply{content: "=== FILE: main.go ===\npackage main\n\nfunc main() { println(\"v2\") }\n=== END FILE ===\n" +
			"=== FILE: go.mod ===\nmodule demo\n\ngo 1.22\n=== END FILE ===\n" + // unchanged
			"=== DELETE: old/notes.md ===\n=== DELETE: never-existed.txt ===\n"}
	})
	runner, rec := newRunner(t, llm, gh)
	task := baseTask(t)
	task.Prompt = "Выведи v2\nвместо v1"
	task.Branch = "feature/v2"
	task.Release = false
	task.Targets = []string{"linux"}

	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, rec.journal())
	}
	if out.Branch != "feature/v2" || out.ReleaseURL != "" {
		t.Errorf("outcome = %+v", out)
	}
	req := llm.request(0)
	for _, want := range []string{"println(\"v1\")", "Stack: Go.", "- logo.png", "- go.sum"} {
		if !strings.Contains(req, want) {
			t.Errorf("request lacks %q:\n%s", want, req)
		}
	}
	if strings.Contains(req, "h1:abc") || strings.Contains(req, "Choose the stack") {
		t.Errorf("request has lock file content or the auto contract:\n%s", req)
	}
	files := gh.headFiles("feature/v2")
	if got, want := sortedKeys(files), []string{".github/workflows/kolibri-build.yml", "go.mod", "go.sum", "logo.png", "main.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if !strings.Contains(files[workflow.Path], `branches: ["feature/v2"]`) || strings.Contains(files[workflow.Path], "release") {
		t.Errorf("workflow:\n%s", files[workflow.Path])
	}
	if _, moved := gh.headFiles("main")[workflow.Path]; moved {
		t.Error("main was touched although the task works in a branch")
	}
	if hist := gh.history("feature/v2"); !strings.HasPrefix(hist[0], "Выведи v2\n\n") {
		t.Errorf("commit message = %q", hist[0])
	}
	// go.mod was repeated unchanged: it must not be listed as a change.
	var changed []string
	for _, f := range rec.files[0] {
		changed = append(changed, f.Status+" "+f.Path)
	}
	want := []string{"new .github/workflows/kolibri-build.yml", "changed main.go", "deleted old/notes.md"}
	if !reflect.DeepEqual(changed, want) {
		t.Errorf("changes = %v, want %v", changed, want)
	}
}

func TestRunAsksAgainWhenReplyHasNoFiles(t *testing.T) {
	gh := newFakeGitHub(t)
	llm := newFakeLLM(t, func(n int, _ []colibri.Message) fakeReply {
		if n == 0 {
			return fakeReply{content: "Конечно! Вот что я бы сделал: написал бы программу."}
		}
		return fakeReply{content: goodReply("package main\n\nfunc main() {}\n")}
	})
	runner, rec := newRunner(t, llm, gh)
	if _, err := runner.Run(context.Background(), baseTask(t)); err != nil {
		t.Fatalf("Run: %v\n%s", err, rec.journal())
	}
	retry := llm.request(1)
	if !strings.Contains(retry, "[assistant]\nКонечно!") || !strings.Contains(retry, "no files in the required format") {
		t.Errorf("retry request:\n%s", retry)
	}
	if llm.count() != 2 {
		t.Errorf("requests = %d", llm.count())
	}
}

func TestRunGivesUpWhenModelNeverSendsFiles(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.seed(map[string]string{"README.md": "# demo\n"})
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply { return fakeReply{content: "Не могу."} })
	runner, _ := newRunner(t, llm, gh)
	_, err := runner.Run(context.Background(), baseTask(t))
	if err == nil || !strings.Contains(err.Error(), "не вернула файлы") {
		t.Fatalf("err = %v", err)
	}
	if hist := gh.history("main"); len(hist) != 1 {
		t.Errorf("nothing should be committed, history = %q", hist)
	}
}

func TestRunStopsAfterAllowedFixes(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.judge = func(map[string]string) (bool, string) { return false, failingLog }
	n := 0
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply {
		n++
		return fakeReply{content: goodReply("package main\n\nfunc main() { BUG() } // v" + strings.Repeat("i", n) + "\n")}
	})
	runner, rec := newRunner(t, llm, gh)
	task := baseTask(t)
	task.MaxFixes = 1
	out, err := runner.Run(context.Background(), task)
	var failed *BuildFailedError
	if !errors.As(err, &failed) || failed.Attempts != 2 {
		t.Fatalf("err = %v\n%s", err, rec.journal())
	}
	if err.Error() != "сборка не прошла после 2 попыток" {
		t.Errorf("message = %q", err.Error())
	}
	if out.RunURL == "" || out.CommitSHA == "" || out.Dir != "" {
		t.Errorf("outcome = %+v", out)
	}
	if llm.count() != 2 {
		t.Errorf("model requests = %d, want 2", llm.count())
	}
}

func TestRunContinuesReplyCutByTokenLimit(t *testing.T) {
	gh := newFakeGitHub(t)
	full := goodReply("package main\n\nfunc main() {}\n")
	cut := strings.Index(full, "func main")
	llm := newFakeLLM(t, func(n int, messages []colibri.Message) fakeReply {
		if n == 0 {
			return fakeReply{content: full[:cut], finish: "length"}
		}
		last := messages[len(messages)-1]
		if last.Role != "assistant" || last.Content != strings.TrimRight(full[:cut], "\n") {
			t.Errorf("continuation request ends with %q %q", last.Role, last.Content)
		}
		// A model resumes from exactly the bytes it was given.
		return fakeReply{content: full[len(last.Content):]}
	})
	runner, rec := newRunner(t, llm, gh)
	if _, err := runner.Run(context.Background(), baseTask(t)); err != nil {
		t.Fatalf("Run: %v\n%s", err, rec.journal())
	}
	if got := gh.headFiles("main")["main.go"]; got != "package main\n\nfunc main() {}\n" {
		t.Errorf("main.go = %q", got)
	}
}

func TestRunShrinksPromptWhenContextOverflows(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.seed(map[string]string{"go.mod": "module demo\n", "big.go": "package main\n// " + strings.Repeat("x", 3000) + "\n"})
	llm := newFakeLLM(t, func(n int, _ []colibri.Message) fakeReply {
		if n < 2 {
			return fakeReply{status: 400, errCode: "context_length_exceeded"}
		}
		return fakeReply{content: "=== FILE: main.go ===\npackage main\n\nfunc main() {}\n=== END FILE ===\n"}
	})
	runner, rec := newRunner(t, llm, gh)
	if _, err := runner.Run(context.Background(), baseTask(t)); err != nil {
		t.Fatalf("Run: %v\n%s", err, rec.journal())
	}
	if first, last := llm.request(0), llm.request(2); !strings.Contains(first, "xxxx") || strings.Contains(last, "xxxx") || !strings.Contains(last, "- big.go") {
		t.Errorf("sources were not dropped from the retried request")
	}
}

func TestRunCancelDuringGeneration(t *testing.T) {
	gh := newFakeGitHub(t)
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply { return fakeReply{} })
	llm.stall = make(chan struct{})
	defer close(llm.stall)
	runner, rec := newRunner(t, llm, gh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel as soon as the first piece of the answer has streamed in.
		for i := 0; i < 2000; i++ {
			rec.mu.Lock()
			got := rec.model.Len() > 0
			rec.mu.Unlock()
			if got {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	done := make(chan error, 1)
	go func() { _, err := runner.Run(ctx, baseTask(t)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if hist := gh.history("main"); len(hist) != 1 {
		t.Errorf("history after cancel = %q", hist)
	}
}

func TestRunReportsMissingWorkflowPermission(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.denyWorkflow = true
	gh.scopes = "repo"
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply {
		return fakeReply{content: goodReply("package main\n\nfunc main() {}\n")}
	})
	runner, rec := newRunner(t, llm, gh)
	_, err := runner.Run(context.Background(), baseTask(t))
	var werr *ghapi.WriteError
	if !errors.As(err, &werr) || !werr.Workflow {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(rec.journal(), "нет области workflow") {
		t.Errorf("no early warning about the scope:\n%s", rec.journal())
	}
}

func TestRunReportsDisabledActions(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.noRuns = true
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply {
		return fakeReply{content: goodReply("package main\n\nfunc main() {}\n")}
	})
	runner, _ := newRunner(t, llm, gh)
	runner.RunStartTimeout = 30 * time.Millisecond
	_, err := runner.Run(context.Background(), baseTask(t))
	if err == nil || !strings.Contains(err.Error(), "settings/actions") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunOnEmptyRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.exists, gh.empty = true, true
	llm := newFakeLLM(t, func(int, []colibri.Message) fakeReply {
		return fakeReply{content: "=== FILE: index.html ===\n<!doctype html><title>x</title>\n=== END FILE ===\n"}
	})
	runner, rec := newRunner(t, llm, gh)
	task := baseTask(t)
	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, rec.journal())
	}
	if out.Kind != workflow.Static {
		t.Errorf("kind = %s", out.Kind)
	}
	if got := sortedKeys(gh.headFiles("main")); len(got) != 3 {
		t.Errorf("files = %v", got)
	}
}

func TestRunFailsFastWithoutColibri(t *testing.T) {
	gh := newFakeGitHub(t)
	runner, _ := newRunner(t, newFakeLLM(t, nil), gh)
	runner.LLM.BaseURL = "http://127.0.0.1:1"
	_, err := runner.Run(context.Background(), baseTask(t))
	if err == nil || !strings.Contains(err.Error(), "нет связи с Колибри") {
		t.Fatalf("err = %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("GitHub was contacted although no code can be generated: %v", gh.calls)
	}
}

func TestExtractFailure(t *testing.T) {
	got := ExtractFailure(failingLog, 4000)
	want := "$ go build -o dist/ ./...\n# demo\n./main.go:5:2: undefined: BUG\nERROR: Process completed with exit code 1."
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	long := "##[group]Run make\n##[endgroup]\n" + strings.Repeat("line of output\n", 2000) + "final: boom\n##[error]exit 2\n"
	short := ExtractFailure(long, 1000)
	if len(short) > 1100 || !strings.HasPrefix(short, "$ make\n") || !strings.Contains(short, "final: boom") || !strings.Contains(short, "log shortened") {
		t.Errorf("squeezed log (%d bytes):\n%s", len(short), short)
	}
	if tail := ExtractFailure("just\nsome\ntext", 100); tail != "just\nsome\ntext" {
		t.Errorf("log without markers = %q", tail)
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{1: "файл", 2: "файла", 5: "файлов", 11: "файлов", 21: "файл", 104: "файла", 112: "файлов"} {
		if got := plural(n, "файл", "файла", "файлов"); got != want {
			t.Errorf("plural(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestFileBudget(t *testing.T) {
	if b := fileBudget(65536, 0, 4000); b < 100_000 || b > 140_000 {
		t.Errorf("64k context: budget = %d", b)
	}
	if b := fileBudget(0, 0, 4000); b < 20_000 || b > 30_000 {
		t.Errorf("unknown context: budget = %d", b)
	}
	if b := fileBudget(16384, 16000, 60000); b != 0 {
		t.Errorf("overfull prompt: budget = %d", b)
	}
}
