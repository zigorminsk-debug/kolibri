package lightpanda

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperLightpanda acts as a stand-in `lightpanda` executable when
// GO_LIGHTPANDA_HELPER=1, speaking the exact newline-delimited MCP protocol.
func TestHelperLightpanda(t *testing.T) {
	if os.Getenv("GO_LIGHTPANDA_HELPER") != "1" {
		return
	}
	args := os.Args[1:]
	// Strip the `go test` flags before "--".
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) > 0 && args[0] == "version" {
		fmt.Println("1.0.4")
		os.Exit(0)
	}
	if len(args) == 0 || args[0] != "mcp" {
		fmt.Fprintf(os.Stderr, "unknown command: %v\n", args)
		os.Exit(1)
	}

	curURL := "about:blank"
	curTitle := ""
	curMD := ""
	filled := ""
	br := bufio.NewReaderSize(os.Stdin, 64<<10)
	reply := func(id json.RawMessage, result any) {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		os.Stdout.Write(append(data, '\n'))
	}
	toolText := func(id json.RawMessage, text string, isErr bool, pageURL, title string, status int) {
		res := map[string]any{
			"content": []map[string]string{{"type": "text", "text": text}},
			"isError": isErr,
		}
		if pageURL != "" {
			res["structuredContent"] = map[string]any{"url": pageURL, "title": title, "httpStatus": status}
		}
		reply(id, res)
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			os.Exit(0)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &req) != nil {
			continue
		}
		// Emit an out-of-band MCP notification to prove the client skips it.
		os.Stdout.WriteString(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"tick"}}` + "\n")

		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "lightpanda", "version": "1.0.4"},
			})
		case "notifications/initialized", "notifications/cancelled":
		case "tools/call":
			a := req.Params.Arguments
			switch req.Params.Name {
			case "search":
				q, _ := a["query"].(string)
				if strings.Contains(q, "rate-limit") {
					toolText(req.ID, "keenable search failed (HTTP 429). This engine is rate-limited right now.", true, "", "", 0)
					continue
				}
				md := fmt.Sprintf("1. **Radmin VPN Overview** — https://www.radmin-vpn.com/\n   Virtual LAN mesh over encrypted tunnel for %s.\n\n2. **WireGuard Protocol** — https://www.wireguard.com/protocol/\n   Modern Noise-based VPN tunnel.\n\n3. **Extra Hit** — https://example.org/3\n   Third entry.\n", q)
				toolText(req.ID, md, false, "", "", 0)
			case "goto":
				u, _ := a["url"].(string)
				switch {
				case strings.Contains(u, "redirect-private"):
					curURL = "http://169.254.169.254/latest/meta-data"
					toolText(req.ID, "Navigated", false, curURL, "Metadata", 200)
				case strings.Contains(u, "ddg-fallback"):
					curURL, curTitle = u, "DuckDuckGo"
					toolText(req.ID, "Navigated", false, curURL, curTitle, 200)
				case strings.Contains(u, "/404"):
					curURL = u
					toolText(req.ID, "Not Found", false, u, "404", 404)
				case strings.Contains(u, "/big"):
					curURL, curTitle = u, "Huge Doc"
					curMD = strings.Repeat("Документация по протоколу туннеля. ", 4500) // > 250 KB UTF-8
					toolText(req.ID, "Navigated", false, curURL, curTitle, 200)
				default:
					curURL, curTitle = u, "Radmin VPN Docs"
					curMD = "# Radmin VPN\n\nCreates a virtual local network with AES-256 encryption and peer-to-peer UDP tunnels."
					toolText(req.ID, "Navigated", false, curURL, curTitle, 200)
				}
			case "getUrl":
				toolText(req.ID, curURL, false, "", "", 0)
			case "markdown":
				sel, _ := a["selector"].(string)
				if sel != "" {
					toolText(req.ID, "## Section "+sel+"\nSelected content.", false, "", "", 0)
					continue
				}
				toolText(req.ID, curMD, false, "", "", 0)
			case "links":
				toolText(req.ID, `[{"text":"Architecture","href":"https://www.radmin-vpn.com/arch","backendNodeId":7},{"text":"Security","href":"https://www.radmin-vpn.com/security","backendNodeId":9}]`, false, "", "", 0)
			case "extract":
				if strings.Contains(curURL, "ddg-fallback") {
					toolText(req.ID, `{"results":[`+
						`{"title":"Ad","url":"https://duckduckgo.com/y.js?ad_domain=spam.test","snippet":"buy"},`+
						`{"title":"Fallback Guide","url":"//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fvpn-guide&rut=abc","snippet":"Step-by-step mesh VPN design."},`+
						`{"title":"Local Trap","url":"//duckduckgo.com/l/?uddg=http%3A%2F%2F10.0.0.1%2Fadmin","snippet":"blocked"}`+
						`]}`, false, "", "", 0)
					continue
				}
				schema, _ := a["schema"].(string)
				toolText(req.ID, `{"items":["AES-256","UDP hole punching"],"schemaLen":`+fmt.Sprint(len(schema))+`}`, false, "", "", 0)
			case "click":
				curURL = "https://www.radmin-vpn.com/arch"
				curTitle = "Architecture"
				curMD = "# Architecture\n\nControl plane + direct P2P data plane."
				toolText(req.ID, "Clicked element", false, curURL, curTitle, 200)
			case "fill":
				filled, _ = a["value"].(string)
				toolText(req.ID, "Filled input", false, curURL, curTitle, 200)
			case "press":
				key, _ := a["key"].(string)
				curURL = "https://www.radmin-vpn.com/search?q=" + filled
				curTitle = "Search: " + filled
				curMD = "Found topic: " + filled + " (key=" + key + ")"
				toolText(req.ID, "Pressed "+key, false, curURL, curTitle, 200)
			case "waitForState", "waitForSelector", "waitForScript":
				toolText(req.ID, "Ready", false, "", "", 0)
			case "tree":
				toolText(req.ID, "[document @1]\n  [heading \"Architecture\" @2]", false, "", "", 0)
			case "interactiveElements":
				toolText(req.ID, `[{"backendNodeId":12,"tagName":"input","name":"q"}]`, false, "", "", 0)
			case "structuredData":
				toolText(req.ID, `{"openGraph":[{"key":"og:title","value":"Radmin VPN"}]}`, false, "", "", 0)
			case "evaluate":
				script, _ := a["script"].(string)
				if script == "hang" {
					time.Sleep(10 * time.Second)
					toolText(req.ID, "late", false, curURL, curTitle, 200)
					continue
				}
				if script == "pid" {
					toolText(req.ID, fmt.Sprint(os.Getpid()), false, curURL, curTitle, 200)
					continue
				}
				if script == "env" {
					out := fmt.Sprintf("telemetry=%s;brave=%s;gh=%s;coli=%s;args=%s",
						os.Getenv("LIGHTPANDA_DISABLE_TELEMETRY"),
						os.Getenv("BRAVE_API_KEY"),
						os.Getenv("GITHUB_TOKEN"),
						os.Getenv("COLI_API_KEY"),
						strings.Join(args, " "),
					)
					toolText(req.ID, out, false, curURL, curTitle, 200)
					continue
				}
				toolText(req.ID, "eval:"+script, false, curURL, curTitle, 200)
			case "html":
				toolText(req.ID, "<article><h1>Radmin VPN</h1></article>", false, "", "", 0)
			case "scroll":
				toolText(req.ID, "Scrolled", false, curURL, curTitle, 200)
			default:
				reply(req.ID, nil)
			}
		}
	}
}

// writeHelperWrapper creates a small script that invokes TestHelperLightpanda
// as if it were the real `lightpanda` executable.
func writeHelperWrapper(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper helper is POSIX-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "lightpanda")
	script := fmt.Sprintf("#!/bin/sh\nGO_LIGHTPANDA_HELPER=1 exec %q -test.run=^TestHelperLightpanda$ -- \"$@\"\n", exe)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStdioSessionActionsAndEnvIsolation(t *testing.T) {
	bin := writeHelperWrapper(t)
	t.Setenv("GITHUB_TOKEN", "ghp_super_secret")
	t.Setenv("GH_TOKEN", "ghp_other_secret")
	t.Setenv("COLI_API_KEY", "coli_secret")

	b := &Browser{
		BinaryPath:        bin,
		SearchProvider:    "brave",
		SearchAPIKey:      "bsa_test_key",
		ObeyRobots:        true,
		ActionTimeout:     10 * time.Second,
		MaxResultBytes:    4000,
		AllowLoopback:     false,
		SearchFallbackURL: "https://example.com/ddg-fallback",
	}

	ctx := context.Background()
	st, err := b.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if st.Version != "1.0.4" || st.Mode != ModeLocal {
		t.Fatalf("status = %+v", st)
	}

	sess, err := b.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	// 1. Subprocess environment must carry telemetry-off and the search key,
	// and never leak GitHub or Colibri secrets.
	envRes := sess.Do(ctx, Action{Kind: "evaluate", Script: "env"})
	if !envRes.OK() {
		t.Fatalf("evaluate env: %s", envRes.Err)
	}
	for _, want := range []string{"telemetry=true", "brave=bsa_test_key", "gh=;", "coli=;", "--obey-robots", "--block-urls"} {
		if !strings.Contains(envRes.Content, want) {
			t.Errorf("env check lacks %q: %s", want, envRes.Content)
		}
	}
	if strings.Contains(envRes.Content, "ghp_super_secret") || strings.Contains(envRes.Content, "coli_secret") {
		t.Fatalf("secret leaked into Lightpanda env: %s", envRes.Content)
	}

	// 2. Native search with result limit.
	sr := sess.Do(ctx, Action{Kind: "search", Query: "radmin vpn", Limit: 2})
	if !sr.OK() || !strings.Contains(sr.Content, "Radmin VPN Overview") || strings.Contains(sr.Content, "Extra Hit") {
		t.Errorf("search = %+v", sr)
	}

	// 3. Automatic DuckDuckGo fallback on 429: unwraps uddg=, drops ads and private IPs.
	fb := sess.Do(ctx, Action{Kind: "search", Query: "rate-limit query"})
	if !fb.OK() || !strings.Contains(fb.Content, "https://example.com/vpn-guide") || !strings.Contains(fb.Content, "Fallback Guide") {
		t.Fatalf("fallback search = %+v", fb)
	}
	if strings.Contains(fb.Content, "spam.test") || strings.Contains(fb.Content, "10.0.0.1") {
		t.Errorf("fallback kept ad or private link:\n%s", fb.Content)
	}

	// 4. Open, links, extract, click, fill+submit, tree, elements, structured_data.
	op := sess.Do(ctx, Action{Kind: "open", URL: "https://example.com/docs"})
	if !op.OK() || !strings.Contains(op.Content, "AES-256") || !strings.Contains(op.Content, "Title: Radmin VPN Docs") {
		t.Errorf("open = %+v", op)
	}
	lnk := sess.Do(ctx, Action{Kind: "links"})
	if !lnk.OK() || !strings.Contains(lnk.Content, "[Architecture](https://www.radmin-vpn.com/arch) [node_id=7]") {
		t.Errorf("links = %+v", lnk)
	}
	ext := sess.Do(ctx, Action{Kind: "extract", Selector: ".feature"})
	if !ext.OK() || !strings.Contains(ext.Content, "UDP hole punching") {
		t.Errorf("extract = %+v", ext)
	}
	clk := sess.Do(ctx, Action{Kind: "click", Selector: "a.arch"})
	if !clk.OK() || !strings.Contains(clk.Content, "Control plane") || clk.URL != "https://www.radmin-vpn.com/arch" {
		t.Errorf("click = %+v", clk)
	}
	fl := sess.Do(ctx, Action{Kind: "fill", Selector: "input[name=q]", Value: "mesh", Submit: true})
	if !fl.OK() || !strings.Contains(fl.Content, "Found topic: mesh") {
		t.Errorf("fill+submit = %+v", fl)
	}
	if r := sess.Do(ctx, Action{Kind: "tree"}); !r.OK() || !strings.Contains(r.Content, "heading") {
		t.Errorf("tree = %+v", r)
	}
	if r := sess.Do(ctx, Action{Kind: "elements"}); !r.OK() || !strings.Contains(r.Content, "backendNodeId") {
		t.Errorf("elements = %+v", r)
	}
	if r := sess.Do(ctx, Action{Kind: "structured_data"}); !r.OK() || !strings.Contains(r.Content, "og:title") {
		t.Errorf("structured_data = %+v", r)
	}

	// 5. Huge UTF-8 line (>250 KB over stdio) is read intact and clipped cleanly on a rune boundary.
	big := sess.Do(ctx, Action{Kind: "open", URL: "https://example.com/big"})
	if !big.OK() || !strings.Contains(big.Content, "содержимое сокращено") {
		t.Errorf("big page = %+v", big)
	}

	// 6. HTTP 404 is reported as an error the model can act on.
	if r := sess.Do(ctx, Action{Kind: "open", URL: "https://example.com/404"}); r.OK() || !strings.Contains(r.Err, "404") {
		t.Errorf("404 = %+v", r)
	}

	// 7. A redirect landing on cloud metadata is blocked and cleared.
	if r := sess.Do(ctx, Action{Kind: "open", URL: "https://example.com/redirect-private"}); r.OK() || !strings.Contains(r.Err, "заблокирован") {
		t.Errorf("redirect to metadata = %+v", r)
	}

	// 8. html, scroll, wait, press, and DedupKey.
	if r := sess.Do(ctx, Action{Kind: "html", URL: "https://example.com/docs", Selector: "article"}); !r.OK() || !strings.Contains(r.Content, "<article>") {
		t.Errorf("html = %+v", r)
	}
	if r := sess.Do(ctx, Action{Kind: "scroll", Y: 600}); !r.OK() {
		t.Errorf("scroll = %+v", r)
	}
	if r := sess.Do(ctx, Action{Kind: "wait", Selector: "#ready"}); !r.OK() {
		t.Errorf("wait = %+v", r)
	}
	if r := sess.Do(ctx, Action{Kind: "press", Key: "Escape"}); !r.OK() {
		t.Errorf("press = %+v", r)
	}
	if k1, k2 := (Action{Kind: "search", Query: " Radmin  VPN "}).DedupKey(), (Action{Kind: "web_search", Query: "radmin vpn"}).DedupKey(); k1 == "" || k1 != k2 {
		t.Errorf("DedupKey mismatch: %q vs %q", k1, k2)
	}
	if k := (Action{Kind: "click", Selector: "a"}).DedupKey(); k != "" {
		t.Errorf("stateful click should not have DedupKey, got %q", k)
	}

	// 9. Timed-out stdio call kills the stuck worker and respawns cleanly for the next step.
	pid1 := sess.Do(ctx, Action{Kind: "evaluate", Script: "pid"}).Content
	shortCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	hung := sess.Do(shortCtx, Action{Kind: "evaluate", Script: "hang"})
	if hung.OK() {
		t.Fatalf("expected timeout on hung call, got %+v", hung)
	}
	pid2 := sess.Do(ctx, Action{Kind: "evaluate", Script: "pid"}).Content
	if pid2 == "" || pid2 == pid1 {
		t.Errorf("expected respawned PID after timeout: before=%q after=%q", pid1, pid2)
	}
}

func TestHTTPMCPTransportSessionRecoveryAndSSE(t *testing.T) {
	var mu sync.Mutex
	seq := 0
	expired := map[string]bool{}
	var deleted []string
	var sawOrigin string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if o := r.Header.Get("Origin"); o != "" {
			sawOrigin = o
			http.Error(w, "Origin not allowed", http.StatusForbidden)
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		if r.Method == http.MethodDelete {
			deleted = append(deleted, sid)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.URL.Path != "/mcp" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.NotFound(w, r)
			return
		}
		if sid != "" && expired[sid] {
			http.Error(w, "Session ended, start a new one", http.StatusNotFound)
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			seq++
			sid = fmt.Sprintf("s%d", seq)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", sid)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"lightpanda","version":"1.0.4"}}}`, *req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Answer as an SSE stream with heartbeats and notifications, like
			// Lightpanda's HttpServer does on long calls.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Mcp-Session-Id", sid)
			fmt.Fprint(w, ": keep-alive\n\n")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"data\":\"hi\"}}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"1. **Hit** — https://example.org (session %s)\\n   ok\"}]}}\n\n", *req.ID, sid)
		}
	}))
	defer srv.Close()

	b := &Browser{MCPURL: srv.URL}
	st, err := b.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if st.Version != "1.0.4" || st.Mode != ModeHTTP || st.Label() != "1.0.4 · MCP" {
		t.Errorf("status = %+v (%s)", st, st.Label())
	}

	sess, err := b.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r1 := sess.Do(context.Background(), Action{Kind: "search", Query: "vpn"})
	if !r1.OK() || !strings.Contains(r1.Content, "session s2") {
		t.Fatalf("first call = %+v", r1)
	}

	// Simulate Lightpanda reaping s2 while Colibri was thinking for 20 minutes.
	mu.Lock()
	expired["s2"] = true
	mu.Unlock()

	r2 := sess.Do(context.Background(), Action{Kind: "search", Query: "vpn again"})
	if !r2.OK() || !strings.Contains(r2.Content, "session s3") {
		t.Fatalf("after session expiry = %+v", r2)
	}
	_ = sess.Close()

	mu.Lock()
	defer mu.Unlock()
	if sawOrigin != "" {
		t.Errorf("unexpected Origin header %q", sawOrigin)
	}
	if len(deleted) != 2 || deleted[0] != "s1" || deleted[1] != "s3" {
		t.Errorf("deleted sessions = %v, want [s1 s3]", deleted)
	}

	// Pointing at a ws:// CDP endpoint gives a clear explanation.
	if _, err := NormalizeMCPURL("ws://127.0.0.1:9222"); err == nil || !strings.Contains(err.Error(), "lightpanda mcp") {
		t.Errorf("ws:// err = %v", err)
	}

	// HTTP 403 (Host/Origin check) and non-MCP 404 surface actionable diagnostics.
	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusForbidden, "403"},
		{http.StatusNotFound, "MCP"},
	} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", tc.code)
		}))
		_, err := (&Browser{MCPURL: bad.URL}).Check(context.Background())
		bad.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d -> err %v, want %q", tc.code, err, tc.want)
		}
	}
}

func TestCheckURLBlocksLocalAndPrivateNetworks(t *testing.T) {
	ctx := context.Background()
	blocked := []string{
		"",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ftp://example.com/a",
		"https://user:pass@example.com/",
		"http://localhost:8000/v1/models",
		"http://sub.localhost/",
		"http://nas.local/",
		"http://router.lan/",
		"http://127.0.0.1:8000/",
		"http://127.0.1.5/",
		"http://[::1]:8000/",
		"http://[::ffff:127.0.0.1]:8000/",
		"http://0.0.0.0/",
		"http://10.1.2.3/",
		"http://172.16.0.1/",
		"http://172.31.255.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/",
		"http://[fe80::1]/",
		"http://[fd00::1]/",
		"http://2130706433/",
		"http://0x7f000001/",
		"http://0177.0.0.1/",
		"http://127.1/",
	}
	for _, raw := range blocked {
		if _, err := CheckURL(ctx, raw, false, true); err == nil {
			t.Errorf("CheckURL(%q) succeeded, want block", raw)
		}
	}
	allowed := []string{
		"https://www.radmin-vpn.com/",
		"https://github.com/lightpanda-io/browser",
		"http://93.184.216.34/docs",
		"http://172.32.0.1/",
	}
	for _, raw := range allowed {
		if _, err := CheckURL(ctx, raw, false, true); err != nil {
			t.Errorf("CheckURL(%q) = %v, want ok", raw, err)
		}
	}
	// Test mode allows loopback only — never private or link-local metadata.
	if _, err := CheckURL(ctx, "http://127.0.0.1:54321/page", true, true); err != nil {
		t.Errorf("allowLoopback 127.0.0.1: %v", err)
	}
	if _, err := CheckURL(ctx, "http://169.254.169.254/", true, true); err == nil {
		t.Error("allowLoopback must still block cloud metadata")
	}
}

func TestInstallDownloadsAndVerifiesBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX script binary test")
	}
	var storageAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth = r.Header.Get("Authorization")
		w.Write([]byte("#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 1.0.4; exit 0; fi\nexit 1\n"))
	}))
	defer storage.Close()

	wantAsset := AssetName("linux", "amd64")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/lightpanda-io/browser/releases":
			_ = json.NewEncoder(w).Encode([]ghRelease{
				{TagName: "1.0.5-rc1", Prerelease: true, Assets: []ghAsset{{Name: wantAsset, URL: "http://" + r.Host + "/dl/rc"}}},
				{TagName: "1.0.4", Assets: []ghAsset{{Name: wantAsset, URL: "http://" + r.Host + "/dl/1.0.4", Size: 68}}},
			})
		case "/dl/1.0.4":
			http.Redirect(w, r, storage.URL+"/lightpanda-x86_64-linux", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	dir := t.TempDir()
	b := &Browser{
		ManagedDir:  dir,
		ReleasesURL: api.URL,
		GOOS:        "linux",
		GOARCH:      "amd64",
	}
	if _, err := b.Check(context.Background()); !errors.Is(err, ErrNotInstalled) {
		// Ignore if the host happens to have lightpanda in PATH; test with empty PATH.
		t.Setenv("PATH", "/bin:/usr/bin")
		if _, err := b.Check(context.Background()); !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("before install: err = %v", err)
		}
	}

	var stages []string
	st, err := b.Install(context.Background(), func(_, _ int64, stage string) {
		stages = append(stages, stage)
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if st.Version != "1.0.4" || st.Mode != ModeLocal || st.Location != filepath.Join(dir, "lightpanda") {
		t.Errorf("installed status = %+v", st)
	}
	if storageAuth != "" {
		t.Errorf("storage saw Authorization %q", storageAuth)
	}
	if len(stages) < 2 {
		t.Errorf("progress stages = %v", stages)
	}
	// Check now finds the managed binary automatically.
	st2, err := b.Check(context.Background())
	if err != nil || st2.Version != "1.0.4" {
		t.Errorf("Check after Install: st=%+v err=%v", st2, err)
	}
}

// TestRealLightpanda exercises a real Lightpanda release binary end to end
// (both stdio MCP and `lightpanda mcp --port` HTTP modes) against a local site
// with JavaScript rendering, links, extraction, clicks and form submission.
// Set LIGHTPANDA_TEST_BIN=/path/to/lightpanda to run it (CI does).
func TestRealLightpanda(t *testing.T) {
	bin := strings.TrimSpace(os.Getenv("LIGHTPANDA_TEST_BIN"))
	if bin == "" {
		t.Skip("LIGHTPANDA_TEST_BIN not set")
	}

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/":
			fmt.Fprintf(w, `<!doctype html><html><head><title>Radmin VPN Reference</title></head><body>
				<main>
					<h1>Radmin VPN Architecture</h1>
					<p id="js-note">loading</p>
					<ul><li class="feat">Mesh virtual LAN</li><li class="feat">AES-256 tunnel</li></ul>
					<a id="more" href="/protocol">Protocol Details</a>
					<form action="/find" method="get"><input id="q" name="q" type="text"><button type="submit">Go</button></form>
				</main>
				<script>document.getElementById('js-note').textContent = 'Peer-to-peer UDP with automatic relay fallback';</script>
			</body></html>`)
		case "/protocol":
			fmt.Fprint(w, `<!doctype html><html><head><title>Protocol Details</title></head><body>
				<article><h1>Tunnel Handshake</h1><p>X25519 key exchange with ChaCha20-Poly1305 frames.</p></article>
			</body></html>`)
		case "/find":
			fmt.Fprintf(w, `<!doctype html><html><head><title>Found</title></head><body>
				<main><p>Query answered: %s</p></main>
			</body></html>`, r.URL.Query().Get("q"))
		case "/ddg":
			fmt.Fprintf(w, `<!doctype html><html><head><title>Search</title></head><body>
				<div class="result">
					<a class="result__a" href="http://%s/">Radmin VPN Reference</a>
					<a class="result__snippet">How virtual LAN mesh networking works for %s.</a>
				</div>
			</body></html>`, r.Host, r.URL.Query().Get("q"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()

	exercise := func(t *testing.T, b *Browser) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		st, err := b.Check(ctx)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		t.Logf("verified %s", st.Detail())

		sess, err := b.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer sess.Close()

		// Open with real V8 JS execution.
		r := sess.Do(ctx, Action{Kind: "open", URL: site.URL + "/"})
		if !r.OK() || !strings.Contains(r.Content, "Peer-to-peer UDP with automatic relay fallback") {
			t.Fatalf("open (JS): %+v", r)
		}
		// Links on the live page.
		lnk := sess.Do(ctx, Action{Kind: "links"})
		if !lnk.OK() || !strings.Contains(lnk.Content, "Protocol Details") {
			t.Fatalf("links: %+v", lnk)
		}
		// CSS-selector extraction.
		ext := sess.Do(ctx, Action{Kind: "extract", Selector: ".feat"})
		if !ext.OK() || !strings.Contains(ext.Content, "Mesh virtual LAN") || !strings.Contains(ext.Content, "AES-256 tunnel") {
			t.Fatalf("extract: %+v", ext)
		}
		// Evaluate JS.
		ev := sess.Do(ctx, Action{Kind: "evaluate", Script: "document.querySelectorAll('.feat').length"})
		if !ev.OK() || !strings.Contains(ev.Content, "2") {
			t.Fatalf("evaluate: %+v", ev)
		}
		// Click link -> navigates to /protocol and reads updated Markdown.
		clk := sess.Do(ctx, Action{Kind: "click", Selector: "#more"})
		if !clk.OK() || !strings.Contains(clk.Content, "ChaCha20-Poly1305") {
			t.Fatalf("click: %+v", clk)
		}
		// Form fill + Enter submission back on /.
		_ = sess.Do(ctx, Action{Kind: "open", URL: site.URL + "/"})
		fl := sess.Do(ctx, Action{Kind: "fill", Selector: "#q", Value: "nat-traversal", Submit: true})
		if !fl.OK() || !strings.Contains(fl.Content, "Query answered: nat-traversal") {
			t.Fatalf("fill+submit: %+v", fl)
		}
		// Browser search fallback via Lightpanda DOM extraction.
		ddg, ok := sess.(*mcpSession)
		if ok {
			hits, err := ddg.duckDuckGoSearch(ctx, "radmin vpn", 5)
			if err != nil || len(hits) != 1 || hits[0].Title != "Radmin VPN Reference" {
				t.Fatalf("duckDuckGoSearch: hits=%+v err=%v", hits, err)
			}
		}
	}

	t.Run("stdio", func(t *testing.T) {
		exercise(t, &Browser{
			BinaryPath:        bin,
			AllowLoopback:     true,
			SearchFallbackURL: site.URL + "/ddg",
			ActionTimeout:     15 * time.Second,
		})
	})

	t.Run("http", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "mcp", "--host", "127.0.0.1", "--port", "19223", "--log-level", "fatal")
		cmd.Env = append(os.Environ(), "LIGHTPANDA_DISABLE_TELEMETRY=true", "LIGHTPANDA_DISABLE_CORE_DUMP=1")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start lightpanda mcp http: %v", err)
		}
		defer func() {
			cancel()
			_ = cmd.Wait()
		}()
		b := &Browser{
			MCPURL:            "http://127.0.0.1:19223",
			AllowLoopback:     true,
			SearchFallbackURL: site.URL + "/ddg",
			ActionTimeout:     15 * time.Second,
		}
		// Wait until the server begins accepting connections.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := b.Check(ctx); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("lightpanda mcp --port 19223 did not become ready")
			}
			time.Sleep(100 * time.Millisecond)
		}
		exercise(t, b)
	})

	t.Run("hardened-flags", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		sess, err := (&Browser{BinaryPath: bin, ObeyRobots: true}).Open(ctx)
		if err != nil {
			t.Fatalf("Open with default hardened flags: %v", err)
		}
		defer sess.Close()
		if r := sess.Do(ctx, Action{Kind: "open", URL: site.URL + "/"}); r.OK() {
			t.Fatal("default browser must block loopback navigation")
		}
	})
}

func TestCleanMarkdown(t *testing.T) {
	in := strings.Join([]string{
		`# Radmin VPN Architecture`,
		`Peer\-to\-peer UDP with ChaCha20\-Poly1305, C\+\+20 and C\# \(fast\!\) \{v1\}`,
		`\- not a bullet`,
		`\+ also not a bullet`,
		`\# not a heading`,
		`> \- quoted non\-bullet`,
		`\![not an image](https://example.com/a.png)`,
		`Escaped backslash \\- stays and ` + "`code\\-span`" + ` stays`,
		"```sh",
		`echo foo\-bar`,
		"```",
	}, "\n")
	want := strings.Join([]string{
		`# Radmin VPN Architecture`,
		`Peer-to-peer UDP with ChaCha20-Poly1305, C++20 and C# \(fast!\) {v1}`,
		`\- not a bullet`,
		`\+ also not a bullet`,
		`\# not a heading`,
		`> \- quoted non-bullet`,
		`\![not an image](https://example.com/a.png)`,
		`Escaped backslash \\- stays and ` + "`code\\-span`" + ` stays`,
		"```sh",
		`echo foo\-bar`,
		"```",
	}, "\n")
	if got := cleanMarkdown(in); got != want {
		t.Fatalf("cleanMarkdown:\n got: %s\nwant: %s", got, want)
	}
}
