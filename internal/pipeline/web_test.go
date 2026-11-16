package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zigorminsk-debug/kolibri/internal/colibri"
	"github.com/zigorminsk-debug/kolibri/internal/lightpanda"
	"github.com/zigorminsk-debug/kolibri/internal/workflow"
	"github.com/zigorminsk-debug/kolibri/internal/worklog"
)

// Full iterative research -> partial code -> extra lookup -> finish -> commit -> build loop,
// modelled directly on "посмотри как устроена программа radmin vpn и сделай аналогичную".
func TestRunIterativeWebResearchAndMixedCodeRounds(t *testing.T) {
	gh := newFakeGitHub(t)
	web := &fakeWeb{
		do: func(_ int, a lightpanda.Action) lightpanda.Result {
			switch a.Kind {
			case "search":
				if strings.Contains(a.Query, "radmin") {
					return lightpanda.Result{
						Content: "1. **Radmin VPN** — https://www.radmin-vpn.com/\n   Virtual LAN mesh over encrypted UDP tunnels.",
					}
				}
				return lightpanda.Result{
					Content: "1. **UDP Keepalive** — https://example.org/nat\n   Send a 25-second keepalive frame to hold NAT bindings.",
				}
			case "open":
				return lightpanda.Result{
					URL:     a.URL,
					Title:   "Radmin VPN Architecture",
					Content: "# Radmin VPN\nVirtual 10.x subnet, peer discovery coordinator, direct P2P encrypted packets.",
				}
			case "extract":
				return lightpanda.Result{
					URL:     "https://www.radmin-vpn.com/",
					Content: `{"features":["virtual subnet","AES-256","peer discovery"]}`,
				}
			default:
				return lightpanda.Result{Content: "ok"}
			}
		},
	}

	llm := newFakeLLM(t, func(n int, msgs []colibri.Message) fakeReply {
		sys := msgs[0].Content
		prompt := msgs[len(msgs)-1].Content
		switch n {
		case 0:
			if !strings.Contains(sys, "Lightpanda") {
				t.Errorf("turn 0 system prompt lacks Lightpanda instructions")
			}
			return fakeReply{content: "=== NOTES ===\nСначала найду описание архитектуры Radmin VPN.\n=== END NOTES ===\n\n" +
				"=== WEB ===\n" + `{"action":"search","query":"radmin vpn architecture"}` + "\n=== END WEB ===\n"}
		case 1:
			if !strings.Contains(prompt, "https://www.radmin-vpn.com/") {
				t.Fatalf("turn 1 lacks search results:\n%s", prompt)
			}
			return fakeReply{content: "=== NOTES ===\nОткрою страницу Radmin VPN и извлеку список функций.\n=== END NOTES ===\n\n" +
				"=== WEB ===\n" +
				`{"action":"open","url":"https://www.radmin-vpn.com/"}` + "\n" +
				`{"action":"extract","selector":".features"}` + "\n" +
				"=== END WEB ===\n"}
		case 2:
			if !strings.Contains(prompt, "Virtual 10.x subnet") || !strings.Contains(prompt, "peer discovery") {
				t.Fatalf("turn 2 lacks page + extract results:\n%s", prompt)
			}
			// Mixed turn: write go.mod and tunnel.go, and ask Lightpanda one more question
			// (plus a duplicate search that must be served from cache).
			return fakeReply{content: "SUMMARY: Локальный аналог Radmin VPN\n\n" +
				"=== FILE: go.mod ===\nmodule localvpn\n\ngo 1.24\n=== END FILE ===\n\n" +
				"=== FILE: tunnel.go ===\npackage main\n\nconst Subnet = \"10.44.0.0/16\"\n=== END FILE ===\n\n" +
				"=== WEB ===\n" +
				`{"action":"search","query":"radmin vpn architecture"}` + "\n" +
				`{"action":"search","query":"udp nat keepalive interval"}` + "\n" +
				"=== END WEB ===\n"}
		default:
			if !strings.Contains(prompt, "25-second keepalive") {
				t.Fatalf("turn 3 lacks second search result:\n%s", prompt)
			}
			if !strings.Contains(prompt, "=== FILE: tunnel.go ===") {
				t.Fatalf("turn 3 did not retain staged tunnel.go in project context")
			}
			return fakeReply{content: "=== FILE: main.go ===\npackage main\n\nconst KeepaliveSec = 25\n\nfunc main() {}\n=== END FILE ===\n\n" +
				"=== FILE: README.md ===\n# LocalVPN\nЛокальная виртуальная сеть.\n=== END FILE ===\n"}
		}
	})

	runner, rec := newRunner(t, llm, gh)
	runner.Web = web
	task := baseTask(t)
	task.Prompt = "посмотри как устроена программа radmin vpn и сделай аналогичную для локального использования"
	task.UseWeb = true
	task.MaxWebSteps = 8

	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v\njournal:\n%s", err, rec.journal())
	}
	if out.Attempts != 1 || out.WebSteps != 5 {
		t.Fatalf("out = %+v (want Attempts=1, WebSteps=5)", out)
	}
	// Duplicate search was cached, so Lightpanda opened once, ran 4 real actions, and closed once.
	if web.opens != 1 || web.closes != 1 || len(web.actions) != 4 {
		t.Fatalf("opens=%d closes=%d actions=%d", web.opens, web.closes, len(web.actions))
	}
	if !rec.web[3].Cached {
		t.Errorf("step 4 should be marked Cached: %+v", rec.web[3])
	}
	head := gh.headFiles("main")
	for _, want := range []string{"go.mod", "tunnel.go", "main.go", "README.md", workflow.Path, worklog.Path} {
		if _, ok := head[want]; !ok {
			t.Errorf("missing committed file %s; have %v", want, sortedKeys(head))
		}
	}
	if !strings.Contains(head[worklog.Path], "Интернет (Lightpanda):") || !strings.Contains(head[worklog.Path], "www.radmin-vpn.com") {
		t.Errorf("worklog missing Lightpanda trace:\n%s", head[worklog.Path])
	}
}

// During a build-fix attempt the model can also delegate a doc lookup to Lightpanda.
func TestRunWebLookupDuringBuildFixAndLazyOpen(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.judge = func(files map[string]string) (bool, string) {
		if strings.Contains(files["main.go"], "BUG") {
			return false, failingLog
		}
		return true, ""
	}
	web := &fakeWeb{
		do: func(_ int, a lightpanda.Action) lightpanda.Result {
			return lightpanda.Result{Content: "Replace BUG() with runTunnel()."}
		},
	}
	llm := newFakeLLM(t, func(n int, msgs []colibri.Message) fakeReply {
		switch n {
		case 0:
			// Attempt 1 uses no web actions -> Lightpanda must not even start yet.
			return fakeReply{content: goodReply("package main\n\nfunc main() { BUG() }\n")}
		case 1:
			if web.opens != 0 {
				t.Errorf("Lightpanda opened eagerly before any === WEB === request")
			}
			return fakeReply{content: "=== WEB ===\n{\"action\":\"search\",\"query\":\"golang BUG replacement\"}\n=== END WEB ===\n"}
		default:
			if !strings.Contains(msgs[len(msgs)-1].Content, "Replace BUG() with runTunnel()") {
				t.Fatalf("fix turn did not receive web result")
			}
			return fakeReply{content: "SUMMARY: Use documented function\n\n=== FILE: main.go ===\npackage main\n\nfunc runTunnel() {}\nfunc main() { runTunnel() }\n=== END FILE ===\n"}
		}
	})
	runner, _ := newRunner(t, llm, gh)
	runner.Web = web
	task := baseTask(t)
	task.UseWeb = true

	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if out.Attempts != 2 || out.WebSteps != 1 || web.opens != 1 || web.closes != 1 {
		t.Fatalf("out=%+v opens=%d closes=%d", out, web.opens, web.closes)
	}
}

// Reaching MaxWebSteps nudges the model to stop browsing and write the code.
func TestRunWebStepLimitConverges(t *testing.T) {
	gh := newFakeGitHub(t)
	web := &fakeWeb{}
	llm := newFakeLLM(t, func(n int, msgs []colibri.Message) fakeReply {
		switch n {
		case 0:
			return fakeReply{content: "=== WEB ===\n{\"action\":\"search\",\"query\":\"step one\"}\n=== END WEB ===\n"}
		case 1:
			return fakeReply{content: "=== WEB ===\n{\"action\":\"search\",\"query\":\"step two\"}\n=== END WEB ===\n"}
		case 2:
			// Model ignores the "no further rounds" note once; pipeline nudges it.
			return fakeReply{content: "=== WEB ===\n{\"action\":\"search\",\"query\":\"step three\"}\n=== END WEB ===\n"}
		default:
			if !strings.Contains(msgs[len(msgs)-1].Content, "Do not output === WEB === anymore") {
				t.Errorf("expected convergence nudge in turn 3")
			}
			return fakeReply{content: goodReply("package main\n\nfunc main() {}\n")}
		}
	})
	runner, _ := newRunner(t, llm, gh)
	runner.Web = web
	task := baseTask(t)
	task.UseWeb = true
	task.MaxWebSteps = 2

	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(web.actions) != 2 || out.WebSteps != 2 {
		t.Fatalf("executed %d actions (want 2), out=%+v", len(web.actions), out)
	}
}

// If Lightpanda is not installed, the pipeline warns the user and lets Colibri
// finish the project from its own knowledge instead of aborting the task.
func TestRunFallsBackWhenLightpandaMissing(t *testing.T) {
	gh := newFakeGitHub(t)
	web := &fakeWeb{openErr: lightpanda.ErrNotInstalled}
	llm := newFakeLLM(t, func(n int, msgs []colibri.Message) fakeReply {
		if n == 0 {
			return fakeReply{content: "=== WEB ===\n{\"action\":\"search\",\"query\":\"radmin vpn\"}\n=== END WEB ===\n"}
		}
		if !strings.Contains(msgs[len(msgs)-1].Content, "unavailable") {
			t.Errorf("turn 1 should tell model Lightpanda is unavailable")
		}
		return fakeReply{content: goodReply("package main\n\nfunc main() {}\n")}
	})
	runner, rec := newRunner(t, llm, gh)
	runner.Web = web
	task := baseTask(t)
	task.UseWeb = true

	if _, err := runner.Run(context.Background(), task); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(rec.journal(), "Установить Lightpanda") {
		t.Errorf("journal should point user to installer:\n%s", rec.journal())
	}
	if len(rec.web) != 1 || rec.web[0].Err == "" {
		t.Errorf("web steps = %+v", rec.web)
	}
}

// Files written in a mixed round are still saved to GitHub with [skip ci] if a
// later round breaks off due to a server drop.
func TestRunSavesStagedFilesWhenPostWebStreamDrops(t *testing.T) {
	gh := newFakeGitHub(t)
	web := &fakeWeb{}
	llm := newFakeLLM(t, func(n int, _ []colibri.Message) fakeReply {
		if n == 0 {
			return fakeReply{content: "SUMMARY: Начало VPN\n\n=== FILE: go.mod ===\nmodule vpn\n\ngo 1.24\n=== END FILE ===\n\n" +
				"=== WEB ===\n{\"action\":\"search\",\"query\":\"tun api\"}\n=== END WEB ===\n"}
		}
		return fakeReply{status: 502}
	})
	runner, _ := newRunner(t, llm, gh)
	runner.Web = web
	task := baseTask(t)
	task.UseWeb = true

	_, err := runner.Run(context.Background(), task)
	var cut *InterruptedError
	if !errors.As(err, &cut) {
		t.Fatalf("err = %v, want InterruptedError", err)
	}
	if _, ok := gh.headFiles("main")["go.mod"]; !ok {
		t.Fatalf("go.mod was not preserved on GitHub: %v", sortedKeys(gh.headFiles("main")))
	}
}

// A large web result that overflows a small model's context triggers automatic
// budget shrinking and still succeeds.
func TestRunContextExceededShrinksWebBudget(t *testing.T) {
	gh := newFakeGitHub(t)
	web := &fakeWeb{
		do: func(_ int, _ lightpanda.Action) lightpanda.Result {
			return lightpanda.Result{Content: strings.Repeat("protocol spec line\n", 1500)}
		},
	}
	var sizes []int
	llm := newFakeLLM(t, func(n int, msgs []colibri.Message) fakeReply {
		if n == 0 {
			return fakeReply{content: "=== WEB ===\n{\"action\":\"open\",\"url\":\"https://example.org/spec\"}\n=== END WEB ===\n"}
		}
		last := msgs[len(msgs)-1].Content
		sizes = append(sizes, len(last))
		if n == 1 {
			return fakeReply{status: 400, errCode: "context_length_exceeded"}
		}
		return fakeReply{content: goodReply("package main\n\nfunc main() {}\n")}
	})
	runner, _ := newRunner(t, llm, gh)
	runner.Web = web
	task := baseTask(t)
	task.UseWeb = true

	if _, err := runner.Run(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[1] >= sizes[0] {
		t.Fatalf("expected shrunk web prompt on retry, got sizes %v", sizes)
	}
}

// End-to-end integration test driving a real Lightpanda binary through the
// full pipeline: task -> browse JS docs -> draft code -> click deeper doc ->
// finish code -> commit to GitHub -> build -> deliver.
func TestRunWithRealLightpanda(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("LIGHTPANDA_TEST_BIN"))
	if bin == "" {
		t.Skip("LIGHTPANDA_TEST_BIN not set")
	}
	docs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<!doctype html><html><head><title>Radmin VPN Overview</title></head><body>
				<main><h1>Virtual LAN Topology</h1><p id="mode"></p><a id="wire" href="/wire">Wire Format</a></main>
				<script>document.getElementById('mode').textContent = 'Full-mesh P2P over UDP port 51820';</script>
			</body></html>`)
		case "/wire":
			fmt.Fprint(w, `<!doctype html><html><head><title>Wire Format</title></head><body>
				<main><h1>Packet Header</h1><p>Magic bytes 0x4B4C with 24-byte XChaCha20 nonce.</p></main>
			</body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer docs.Close()

	gh := newFakeGitHub(t)
	llm := newFakeLLM(t, func(n int, msgs []colibri.Message) fakeReply {
		last := msgs[len(msgs)-1].Content
		switch n {
		case 0:
			return fakeReply{content: fmt.Sprintf("=== NOTES ===\nИзучаю топологию виртуальной сети.\n=== END NOTES ===\n\n=== WEB ===\n{\"action\":\"open\",\"url\":%q}\n{\"action\":\"links\"}\n=== END WEB ===\n", docs.URL+"/")}
		case 1:
			if !strings.Contains(last, "Full-mesh P2P over UDP port 51820") || !strings.Contains(last, "Wire Format") {
				t.Fatalf("turn 1 missing live Lightpanda page data:\n%s", last)
			}
			return fakeReply{content: "SUMMARY: Локальная виртуальная сеть\n\n" +
				"=== FILE: go.mod ===\nmodule localvpn\n\ngo 1.24\n=== END FILE ===\n\n" +
				"=== WEB ===\n{\"action\":\"click\",\"selector\":\"#wire\"}\n=== END WEB ===\n"}
		default:
			if !strings.Contains(last, "0x4B4C") {
				t.Fatalf("turn 2 missing clicked Wire Format page:\n%s", last)
			}
			return fakeReply{content: "=== FILE: main.go ===\npackage main\n\nconst Magic = 0x4B4C\nconst Port = 51820\n\nfunc main() {}\n=== END FILE ===\n"}
		}
	})

	runner, rec := newRunner(t, llm, gh)
	runner.Web = &lightpanda.Browser{
		BinaryPath:    bin,
		AllowLoopback: true,
		ActionTimeout: 15 * time.Second,
	}
	task := baseTask(t)
	task.Prompt = "посмотри как устроена программа radmin vpn и сделай аналогичную для локального использования"
	task.UseWeb = true

	out, err := runner.Run(context.Background(), task)
	if err != nil {
		t.Fatalf("Run: %v\njournal:\n%s", err, rec.journal())
	}
	if out.WebSteps != 3 || !strings.Contains(gh.headFiles("main")["main.go"], "0x4B4C") {
		t.Fatalf("out=%+v main.go=%q", out, gh.headFiles("main")["main.go"])
	}
}
