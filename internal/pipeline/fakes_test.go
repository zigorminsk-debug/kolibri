package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/lightpanda"
)

// ---- fake colibri -------------------------------------------------------

type fakeReply struct {
	reasoning string
	content   string
	finish    string // defaults to "stop"
	status    int    // non-zero: answer with this HTTP error
	errCode   string
	// drop: the server dies after sending the content — the connection
	// breaks without a finish reason.
	drop bool
}

type fakeLLM struct {
	t        *testing.T
	mu       sync.Mutex
	requests [][]colibri.Message
	// reply decides the answer for the n-th request (from 0).
	reply func(n int, messages []colibri.Message) fakeReply
	// block, when set, is closed-over by tests to stall a stream.
	stall chan struct{}
	srv   *httptest.Server
}

func newFakeLLM(t *testing.T, reply func(n int, messages []colibri.Message) fakeReply) *fakeLLM {
	f := &fakeLLM{t: t, reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen-test","object":"model","owned_by":"colibri","context_length":32768,"max_model_len":32768}]}`)
	case "/v1/chat/completions":
		var body struct {
			Messages []colibri.Message `json:"messages"`
			Stream   bool              `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Stream {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		n := len(f.requests)
		f.requests = append(f.requests, body.Messages)
		f.mu.Unlock()
		rep := f.reply(n, body.Messages)
		if rep.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rep.status)
			fmt.Fprintf(w, `{"error":{"message":"boom","type":"invalid_request_error","code":%q}}`, rep.errCode)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		send := func(delta map[string]string, finish any) {
			chunk := map[string]any{"object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
		// Colibri's keepalive: an empty reasoning delta.
		send(map[string]string{"reasoning_content": ""}, nil)
		fmt.Fprint(w, ": comment line\n\n")
		if rep.reasoning != "" {
			send(map[string]string{"reasoning_content": rep.reasoning}, nil)
		}
		if f.stall != nil {
			send(map[string]string{"content": "partial"}, nil)
			select {
			case <-f.stall:
			case <-r.Context().Done():
			}
			return
		}
		// Split the content in uneven pieces, as tokens would arrive.
		text := []rune(rep.content)
		for i := 0; i < len(text); i += 7 {
			end := min(i+7, len(text))
			send(map[string]string{"content": string(text[i:end])}, nil)
		}
		if rep.drop {
			panic(http.ErrAbortHandler)
		}
		finish := rep.finish
		if finish == "" {
			finish = "stop"
		}
		send(map[string]string{}, finish)
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeLLM) request(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		f.t.Fatalf("model got %d requests, want request #%d", len(f.requests), n)
	}
	var b strings.Builder
	for _, m := range f.requests[n] {
		fmt.Fprintf(&b, "[%s]\n%s\n", m.Role, m.Content)
	}
	return b.String()
}

func (f *fakeLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ---- fake GitHub --------------------------------------------------------

type fakeCommit struct {
	tree    string
	parent  string
	message string
}

type fakeRun struct {
	id     int64
	number int
	sha    string
	polls  int
	ok     bool
	log    string
}

type fakeGitHub struct {
	t   *testing.T
	mu  sync.Mutex
	srv *httptest.Server

	exists  bool
	empty   bool // repository without commits
	blobs   map[string]string
	trees   map[string]map[string]string // tree sha -> path -> blob sha
	modes   map[string]map[string]string // tree sha -> path -> mode
	commits map[string]fakeCommit
	refs    map[string]string // branch -> commit sha
	runs    []*fakeRun
	seq     int

	// judge decides whether a tree builds; the log is used when it does not.
	judge func(files map[string]string) (ok bool, log string)
	// noRuns simulates a repository with Actions disabled.
	noRuns bool
	// denyWorkflow simulates a token without the workflow permission.
	denyWorkflow bool
	// releaseFails makes the Release job of every good build fail with
	// this annotation, as a broken publishing step would.
	releaseFails string
	scopes       string
	calls        []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{
		t: t, blobs: map[string]string{}, trees: map[string]map[string]string{}, modes: map[string]map[string]string{},
		commits: map[string]fakeCommit{}, refs: map[string]string{}, scopes: "repo, workflow",
		judge: func(map[string]string) (bool, string) { return true, "" },
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) id(prefix string) string {
	g.seq++
	return fmt.Sprintf("%s%038d", prefix, g.seq)
}

// seed creates the repository with an initial commit holding files.
func (g *fakeGitHub) seed(files map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.exists = true
	tree := map[string]string{}
	modes := map[string]string{}
	for p, c := range files {
		sha := g.id("b0")
		g.blobs[sha] = c
		tree[p] = sha
		modes[p] = "100644"
	}
	tsha := g.id("70")
	g.trees[tsha], g.modes[tsha] = tree, modes
	csha := g.id("c0")
	g.commits[csha] = fakeCommit{tree: tsha, message: "seed"}
	g.refs["main"] = csha
}

func (g *fakeGitHub) files(commit string) map[string]string {
	out := map[string]string{}
	for p, b := range g.trees[g.commits[commit].tree] {
		out[p] = g.blobs[b]
	}
	return out
}

// headFiles returns the files at the tip of a branch.
func (g *fakeGitHub) headFiles(branch string) map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.files(g.refs[branch])
}

func (g *fakeGitHub) history(branch string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for sha := g.refs[branch]; sha != ""; sha = g.commits[sha].parent {
		out = append(out, g.commits[sha].message)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func ghError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func (g *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer test-token" {
		ghError(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var body map[string]any
	if r.Body != nil && r.Method != http.MethodGet {
		json.NewDecoder(r.Body).Decode(&body)
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	path := r.URL.Path
	repoBase := "/repos/me/demo"

	switch {
	case path == "/user":
		w.Header().Set("X-OAuth-Scopes", g.scopes)
		writeJSON(w, 200, map[string]string{"login": "me"})
		return
	case path == "/user/repos" && r.Method == http.MethodPost:
		if str("name") != "demo" || body["auto_init"] != true {
			ghError(w, 422, "unexpected create payload")
			return
		}
		g.exists = true
		tsha := g.id("70")
		bsha := g.id("b0")
		g.blobs[bsha] = "# demo\n"
		g.trees[tsha] = map[string]string{"README.md": bsha}
		g.modes[tsha] = map[string]string{"README.md": "100644"}
		csha := g.id("c0")
		g.commits[csha] = fakeCommit{tree: tsha, message: "Initial commit"}
		g.refs["main"] = csha
		writeJSON(w, 201, g.repoJSON(body["private"] == true))
		return
	}
	if !strings.HasPrefix(path, repoBase) || !g.exists {
		ghError(w, 404, "Not Found")
		return
	}
	rest := strings.TrimPrefix(path, repoBase)

	switch {
	case rest == "":
		writeJSON(w, 200, g.repoJSON(true))

	case strings.HasPrefix(rest, "/git/ref/heads/"):
		if g.empty {
			ghError(w, 409, "Git Repository is empty.")
			return
		}
		sha, ok := g.refs[strings.TrimPrefix(rest, "/git/ref/heads/")]
		if !ok {
			ghError(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"object": map[string]string{"sha": sha}})

	case strings.HasPrefix(rest, "/git/commits/"):
		c, ok := g.commits[strings.TrimPrefix(rest, "/git/commits/")]
		if !ok {
			ghError(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"tree": map[string]string{"sha": c.tree}})

	case strings.HasPrefix(rest, "/git/trees/"):
		tsha := strings.TrimPrefix(rest, "/git/trees/")
		var entries []map[string]any
		for p, b := range g.trees[tsha] {
			entries = append(entries, map[string]any{"path": p, "mode": g.modes[tsha][p], "type": "blob", "sha": b, "size": len(g.blobs[b])})
		}
		writeJSON(w, 200, map[string]any{"tree": entries, "truncated": false})

	case strings.HasPrefix(rest, "/zipball/"):
		files := g.files(strings.TrimPrefix(rest, "/zipball/"))
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		zw.Create("me-demo-abc1234/")
		for p, c := range files {
			fw, _ := zw.Create("me-demo-abc1234/" + p)
			fw.Write([]byte(c))
		}
		zw.Close()
		w.Header().Set("Content-Type", "application/zip")
		w.Write(buf.Bytes())

	case rest == "/contents/README.md" && r.Method == http.MethodPut:
		data, _ := base64.StdEncoding.DecodeString(str("content"))
		bsha, tsha, csha := g.id("b0"), g.id("70"), g.id("c0")
		g.blobs[bsha] = string(data)
		g.trees[tsha] = map[string]string{"README.md": bsha}
		g.modes[tsha] = map[string]string{"README.md": "100644"}
		g.commits[csha] = fakeCommit{tree: tsha, message: str("message")}
		branch := str("branch")
		if branch == "" {
			branch = "main"
		}
		g.refs[branch] = csha
		g.empty = false
		writeJSON(w, 201, map[string]any{})

	case rest == "/git/refs" && r.Method == http.MethodPost:
		g.refs[strings.TrimPrefix(str("ref"), "refs/heads/")] = str("sha")
		writeJSON(w, 201, map[string]any{})

	case rest == "/git/blobs" && r.Method == http.MethodPost:
		if str("encoding") != "base64" {
			ghError(w, 422, "expected base64")
			return
		}
		data, err := base64.StdEncoding.DecodeString(str("content"))
		if err != nil {
			ghError(w, 422, "bad base64")
			return
		}
		sha := g.id("b0")
		g.blobs[sha] = string(data)
		writeJSON(w, 201, map[string]string{"sha": sha})

	case rest == "/git/trees" && r.Method == http.MethodPost:
		base := str("base_tree")
		tree, modes := map[string]string{}, map[string]string{}
		for p, b := range g.trees[base] {
			tree[p], modes[p] = b, g.modes[base][p]
		}
		items, _ := body["tree"].([]any)
		for _, it := range items {
			item := it.(map[string]any)
			p := item["path"].(string)
			if item["sha"] == nil {
				if _, ok := tree[p]; !ok {
					ghError(w, 422, "GitRPC::BadObjectState")
					return
				}
				delete(tree, p)
				delete(modes, p)
				continue
			}
			tree[p], modes[p] = item["sha"].(string), item["mode"].(string)
		}
		sha := g.id("70")
		g.trees[sha], g.modes[sha] = tree, modes
		writeJSON(w, 201, map[string]string{"sha": sha})

	case rest == "/git/commits" && r.Method == http.MethodPost:
		parents, _ := body["parents"].([]any)
		sha := g.id("c0")
		g.commits[sha] = fakeCommit{tree: str("tree"), parent: parents[0].(string), message: str("message")}
		writeJSON(w, 201, map[string]string{"sha": sha})

	case strings.HasPrefix(rest, "/git/refs/heads/") && r.Method == http.MethodPatch:
		branch := strings.TrimPrefix(rest, "/git/refs/heads/")
		sha := str("sha")
		if g.commits[sha].parent != g.refs[branch] {
			ghError(w, 422, "Update is not a fast forward")
			return
		}
		if g.denyWorkflow {
			if _, ok := g.files(sha)[".github/workflows/kolibri-build.yml"]; ok {
				ghError(w, 404, "Not Found")
				return
			}
		}
		g.refs[branch] = sha
		// Like GitHub, the fake starts no workflow for a [skip ci] commit.
		if !g.noRuns && !strings.Contains(g.commits[sha].message, "[skip ci]") {
			ok, log := g.judge(g.files(sha))
			g.runs = append(g.runs, &fakeRun{id: int64(1000 + len(g.runs)), number: len(g.runs) + 1, sha: sha, ok: ok, log: log})
		}
		writeJSON(w, 200, map[string]any{})

	case rest == "/actions/runs":
		var out []map[string]any
		for _, run := range g.runs {
			if run.sha != r.URL.Query().Get("head_sha") {
				continue
			}
			run.polls++
			out = append(out, g.runJSON(run))
		}
		writeJSON(w, 200, map[string]any{"workflow_runs": out})

	case strings.HasPrefix(rest, "/actions/runs/") && strings.HasSuffix(rest, "/jobs"):
		run := g.findRun(rest)
		status, conclusion := runState(run)
		step := map[string]any{"name": "Build", "number": 3, "status": status, "conclusion": conclusion}
		job := map[string]any{"id": run.id + 5000, "run_id": run.id, "name": "Build (linux)", "status": status, "conclusion": conclusion,
			"html_url": "https://github.test/job", "steps": []any{step}}
		jobs := []any{job}
		if g.releaseFails != "" && run.ok && status == "completed" {
			jobs = append(jobs, map[string]any{"id": run.id + 7000, "run_id": run.id, "name": "Release", "status": "completed", "conclusion": "failure",
				"html_url": "https://github.test/release-job", "steps": []any{map[string]any{"name": "Publish release", "number": 2, "status": "completed", "conclusion": "failure"}}})
		}
		writeJSON(w, 200, map[string]any{"jobs": jobs})

	case strings.HasPrefix(rest, "/check-runs/") && strings.HasSuffix(rest, "/annotations"):
		writeJSON(w, 200, []any{
			map[string]any{"annotation_level": "failure", "message": "Process completed with exit code 1.", "path": ".github"},
			map[string]any{"annotation_level": "failure", "title": "Release", "message": g.releaseFails, "path": ".github"},
			map[string]any{"annotation_level": "notice", "message": "ubuntu-latest will migrate", "path": ".github"},
		})

	case strings.HasPrefix(rest, "/actions/jobs/") && strings.HasSuffix(rest, "/logs"):
		var id int64
		fmt.Sscanf(rest, "/actions/jobs/%d/logs", &id)
		for _, run := range g.runs {
			if run.id+5000 == id {
				w.Write([]byte(run.log))
				return
			}
		}
		ghError(w, 404, "Not Found")

	case strings.HasPrefix(rest, "/actions/runs/") && strings.HasSuffix(rest, "/artifacts"):
		run := g.findRun(rest)
		writeJSON(w, 200, map[string]any{"artifacts": []any{map[string]any{"id": run.id + 9000, "name": "demo-linux", "size_in_bytes": 1234, "expired": false}}})

	case strings.HasPrefix(rest, "/actions/artifacts/") && strings.HasSuffix(rest, "/zip"):
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		fw, _ := zw.Create("demo")
		fw.Write([]byte("\x7fELF fake binary"))
		fw, _ = zw.Create("docs/readme.txt")
		fw.Write([]byte("hello"))
		zw.Close()
		w.Write(buf.Bytes())

	case strings.HasPrefix(rest, "/releases/tags/"):
		if g.releaseFails != "" {
			g.t.Errorf("fake GitHub: a release that was not published must not be looked up")
		}
		tag := strings.TrimPrefix(rest, "/releases/tags/")
		writeJSON(w, 200, map[string]string{"tag_name": tag, "html_url": "https://github.test/me/demo/releases/tag/" + tag})

	default:
		g.t.Errorf("fake GitHub: unexpected %s %s", r.Method, r.URL.Path)
		ghError(w, 404, "Not Found")
	}
}

func (g *fakeGitHub) repoJSON(private bool) map[string]any {
	return map[string]any{"full_name": "me/demo", "default_branch": "main", "private": private, "html_url": "https://github.test/me/demo"}
}

func (g *fakeGitHub) findRun(rest string) *fakeRun {
	var id int64
	fmt.Sscanf(rest, "/actions/runs/%d/", &id)
	for _, run := range g.runs {
		if run.id == id {
			return run
		}
	}
	g.t.Fatalf("fake GitHub: no run %d", id)
	return nil
}

// runState walks a run through queued -> in_progress -> completed.
func runState(run *fakeRun) (status string, conclusion any) {
	switch {
	case run.polls <= 1:
		return "queued", nil
	case run.polls == 2:
		return "in_progress", nil
	case run.ok:
		return "completed", "success"
	}
	return "completed", "failure"
}

func (g *fakeGitHub) runJSON(run *fakeRun) map[string]any {
	status, conclusion := runState(run)
	if g.releaseFails != "" && status == "completed" {
		conclusion = "failure" // one failed job fails the whole run
	}
	return map[string]any{"id": run.id, "name": "Kolibri Build", "status": status, "conclusion": conclusion, "run_number": run.number,
		"head_sha": run.sha, "event": "push", "html_url": fmt.Sprintf("https://github.test/me/demo/actions/runs/%d", run.id)}
}

// ---- recording observer -------------------------------------------------

type recorder struct {
	mu     sync.Mutex
	stages []string
	logs   []string
	model  strings.Builder
	think  strings.Builder
	web    []WebStep
	files  [][]FileChange
	builds int
	onLog  func(string)
}

func (r *recorder) Stage(s Stage, attempt, attempts int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, fmt.Sprintf("%d:%d/%d", s, attempt, attempts))
}

func (r *recorder) Log(level Level, text string) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf("%d %s", level, text))
	cb := r.onLog
	r.mu.Unlock()
	if cb != nil {
		cb(text)
	}
}

func (r *recorder) ModelStart(string) {}

func (r *recorder) ModelDelta(text string, reasoning bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reasoning {
		r.think.WriteString(text)
	} else {
		r.model.WriteString(text)
	}
}

func (r *recorder) Web(steps []WebStep) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.web = append([]WebStep(nil), steps...)
}

func (r *recorder) Files(files []FileChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files = append(r.files, files)
}

func (r *recorder) Build([]RunStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.builds++
}

func (r *recorder) journal() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- fake lightpanda ----------------------------------------------------

type fakeWeb struct {
	mu      sync.Mutex
	opens   int
	closes  int
	actions []lightpanda.Action
	openErr error
	do      func(n int, a lightpanda.Action) lightpanda.Result
}

func (f *fakeWeb) Check(context.Context) (lightpanda.Status, error) {
	if f.openErr != nil {
		return lightpanda.Status{}, f.openErr
	}
	return lightpanda.Status{Mode: lightpanda.ModeLocal, Version: "1.0.4", Location: "fake"}, nil
}

func (f *fakeWeb) Open(context.Context) (lightpanda.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeWebSess{f: f}, nil
}

type fakeWebSess struct{ f *fakeWeb }

func (s *fakeWebSess) Do(_ context.Context, a lightpanda.Action) lightpanda.Result {
	s.f.mu.Lock()
	n := len(s.f.actions)
	s.f.actions = append(s.f.actions, a)
	fn := s.f.do
	s.f.mu.Unlock()
	if fn != nil {
		r := fn(n, a)
		r.Action = a
		if r.Duration == 0 {
			r.Duration = 15 * time.Millisecond
		}
		return r
	}
	return lightpanda.Result{Action: a, Content: "ok", Duration: 10 * time.Millisecond}
}

func (s *fakeWebSess) Close() error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.closes++
	return nil
}
