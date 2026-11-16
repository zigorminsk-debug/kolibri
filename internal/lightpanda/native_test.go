package lightpanda

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsNativeEngineWorksWithoutWSLOrDocker(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	b := &Browser{
		GOOS:            "windows",
		GOARCH:          "amd64",
		ManagedDir:      t.TempDir(),
		DisableJSBridge: true,
	}
	ctx := context.Background()

	st, err := b.Check(ctx)
	if err != nil {
		t.Fatalf("Check on Windows without WSL/Docker failed: %v", err)
	}
	if st.Mode != ModeNative || !strings.Contains(st.Detail(), "встроенный движок для Windows") {
		t.Fatalf("unexpected status: %+v (%s)", st, st.Detail())
	}

	inst, err := b.Install(ctx, nil)
	if err != nil {
		t.Fatalf("Install on Windows without WSL/Docker failed: %v", err)
	}
	if inst.Mode != ModeNative || !strings.Contains(inst.Detail(), "встроенный движок для Windows") {
		t.Fatalf("unexpected install status: %+v (%s)", inst, inst.Detail())
	}
}

func TestNativeEngineFullSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "vpn42", Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html>
<html>
<head>
  <title>Radmin VPN Docs</title>
  <meta name="description" content="Virtual LAN mesh tool for local gaming and work">
  <meta property="og:title" content="Radmin VPN Architecture">
  <link rel="canonical" href="https://docs.example.com/">
  <script type="application/ld+json">{"@type":"SoftwareApplication","name":"RadminVPN"}</script>
  <style>.secret { display: none; }</style>
</head>
<body>
  <nav><a href="/login">Sign in</a> <a href="/pricing">Pricing</a></nav>
  <main>
    <h1>Radmin VPN Overview</h1>
    <p>Connects peers into a <strong>virtual LAN</strong> over encrypted UDP tunnels with automatic NAT traversal and zero port forwarding required on home routers. `+strings.Repeat("Encrypted peer discovery keeps latency low across local and remote subnets. ", 7)+`</p>
    <p style="display:none">HIDDEN_NOISE</p>
    <ul class="features">
      <li class="feat" data-tier="core">Full-mesh WireGuard tunnel</li>
      <li class="feat" data-tier="core">AES-256 &amp; ChaCha20-Poly1305</li>
      <li class="feat skip" data-tier="legacy">Deprecated PPTP</li>
    </ul>
    <table>
      <tr><th>Component</th><th>Port</th></tr>
      <tr><td>UDP Hole Punch</td><td><code>51820</code></td></tr>
      <tr><td>Relay Fallback</td><td><code>443</code></td></tr>
    </table>
    <pre><code class="language-go">conn, err := net.ListenUDP("udp", addr)</code></pre>
    <a id="spec-link" href="/spec">Protocol Specification</a>
    <form action="/lookup" method="post">
      <input type="hidden" name="csrf" value="tok9">
      <label for="q">Topic</label>
      <input id="q" name="q" placeholder="Search docs">
      <button type="submit">Go</button>
    </form>
  </main>
  <footer>Copyright 2026</footer>
</body>
</html>`)
	})
	mux.HandleFunc("/spec", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Protocol Spec</title></head><body>
			<article><h1>Handshake v2</h1><p>Uses X25519 ephemeral keys and 24-byte XChaCha20 nonces.</p></article>
		</body></html>`)
	})
	mux.HandleFunc("/lookup", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		c, _ := r.Cookie("sid")
		sid := ""
		if c != nil {
			sid = c.Value
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><title>Results</title></head><body>
			<main><p>Method=%s Query=%s CSRF=%s Cookie=%s</p></main>
		</body></html>`, r.Method, r.FormValue("q"), r.FormValue("csrf"), sid)
	})
	mux.HandleFunc("/alt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Alt Page</title><link rel="alternate" type="text/markdown" href="/alt.md"></head><body><p>HTML copy</p></body></html>`)
	})
	mux.HandleFunc("/alt.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprint(w, "# Author Markdown\n\nDirect clean spec for AI agents.")
	})
	mux.HandleFunc("/spa-next", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Next App</title></head><body>
			<div id="__next"></div>
			<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"summary":"Peer relay negotiates STUN bindings before falling back to TURN."}}}</script>
		</body></html>`)
	})
	mux.HandleFunc("/api/info.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"protocol":"wireguard","mtu":1420}`)
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nDisallow: /private/\nAllow: /private/public\n")
	})
	mux.HandleFunc("/private/secret", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should be blocked")
	})
	mux.HandleFunc("/private/public", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>Allowed by longer rule</p>")
	})
	mux.HandleFunc("/ddg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		target := "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fwww.radmin-vpn.com%2Fdocs"
		fmt.Fprintf(w, `<html><body><div class="result">
			<a class="result__a" href=%q>Radmin VPN Documentation</a>
			<a class="result__snippet">How to configure virtual local networks for %s.</a>
		</div></body></html>`, target, r.URL.Query().Get("q"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	b := &Browser{
		ForceNative:       true,
		AllowLoopback:     true,
		ObeyRobots:        true,
		DisableJSBridge:   true,
		SearchFallbackURL: srv.URL + "/ddg",
	}
	ctx := context.Background()
	sess, err := b.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	// 1. Search (DuckDuckGo HTML DOM parser + uddg unwrap).
	sr := sess.Do(ctx, Action{Kind: "search", Query: "radmin vpn"})
	if !sr.OK() || !strings.Contains(sr.Content, "Radmin VPN Documentation") || !strings.Contains(sr.Content, "https://www.radmin-vpn.com/docs") {
		t.Fatalf("search: %+v", sr)
	}

	// 2. Open with reader-mode stripping, Markdown headings, tables, code fence.
	op := sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/"})
	if !op.OK() {
		t.Fatalf("open: %+v", op)
	}
	for _, want := range []string{
		"Title: Radmin VPN Docs",
		"# Radmin VPN Overview",
		"**virtual LAN**",
		"ChaCha20-Poly1305",
		"| Component | Port |",
		"| UDP Hole Punch | `51820` |",
		"```go\nconn, err := net.ListenUDP",
		"[Protocol Specification](" + srv.URL + "/spec)",
	} {
		if !strings.Contains(op.Content, want) {
			t.Errorf("open missing %q in:\n%s", want, op.Content)
		}
	}
	for _, unwanted := range []string{"HIDDEN_NOISE", "Sign in", "Copyright 2026"} {
		if strings.Contains(op.Content, unwanted) {
			t.Errorf("open leaked chrome/hidden text %q in:\n%s", unwanted, op.Content)
		}
	}

	// 3. Links, semantic tree, interactive elements, structured data.
	lnk := sess.Do(ctx, Action{Kind: "links"})
	if !lnk.OK() || !strings.Contains(lnk.Content, "Protocol Specification") || !strings.Contains(lnk.Content, "[node_id=") {
		t.Fatalf("links: %+v", lnk)
	}
	tr := sess.Do(ctx, Action{Kind: "tree"})
	if !tr.OK() || !strings.Contains(tr.Content, "[main") || !strings.Contains(tr.Content, "heading:h1") {
		t.Fatalf("tree: %+v", tr)
	}
	el := sess.Do(ctx, Action{Kind: "elements"})
	if !el.OK() || !strings.Contains(el.Content, `name="q"`) || !strings.Contains(el.Content, "Topic") {
		t.Fatalf("elements: %+v", el)
	}
	sd := sess.Do(ctx, Action{Kind: "structured_data"})
	if !sd.OK() || !strings.Contains(sd.Content, "RadminVPN") || !strings.Contains(sd.Content, "og:title") {
		t.Fatalf("structured_data: %+v", sd)
	}

	// 4. CSS3 selector extract (compound + attribute + :not).
	ext := sess.Do(ctx, Action{Kind: "extract", Selector: `ul.features > li.feat[data-tier^="cor" i]:not(.skip)`})
	if !ext.OK() || !strings.Contains(ext.Content, "Full-mesh WireGuard") || strings.Contains(ext.Content, "Deprecated PPTP") {
		t.Fatalf("extract: %+v", ext)
	}

	// 5. Evaluate common DOM expressions.
	ev := sess.Do(ctx, Action{Kind: "evaluate", Script: "document.querySelectorAll('.feat').length"})
	if !ev.OK() || ev.Content != "3" {
		t.Fatalf("evaluate: %+v", ev)
	}

	// 6. POST form submission preserving hidden inputs and session cookies.
	fl := sess.Do(ctx, Action{Kind: "fill", Selector: "#q", Value: "hole-punch", Submit: true})
	if !fl.OK() || !strings.Contains(fl.Content, "Method=POST Query=hole-punch CSRF=tok9 Cookie=vpn42") {
		t.Fatalf("fill+submit: %+v", fl)
	}

	// 7. Click navigation.
	_ = sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/"})
	clk := sess.Do(ctx, Action{Kind: "click", Selector: "#spec-link"})
	if !clk.OK() || !strings.Contains(clk.Content, "X25519 ephemeral keys") {
		t.Fatalf("click: %+v", clk)
	}

	// 8. Markdown alternate link, Next.js SPA JSON hydration & raw JSON endpoint.
	alt := sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/alt"})
	if !alt.OK() || !strings.Contains(alt.Content, "# Author Markdown") {
		t.Fatalf("markdown alternate: %+v", alt)
	}
	spa := sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/spa-next"})
	if !spa.OK() || !strings.Contains(spa.Content, "STUN bindings") {
		t.Fatalf("spa fallback: %+v", spa)
	}
	jsn := sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/api/info.json"})
	if !jsn.OK() || !strings.Contains(jsn.Content, `"mtu": 1420`) {
		t.Fatalf("json open: %+v", jsn)
	}

	// 9. robots.txt enforcement (Disallow vs longer Allow).
	if denied := sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/private/secret"}); denied.OK() {
		t.Fatal("expected robots.txt to block /private/secret")
	}
	if allowed := sess.Do(ctx, Action{Kind: "open", URL: srv.URL + "/private/public"}); !allowed.OK() {
		t.Fatalf("expected longer Allow rule to permit /private/public: %+v", allowed)
	}

	// 10. Network log recorded requests.
	netRes := sess.Do(ctx, Action{Kind: "network"})
	if !netRes.OK() || !strings.Contains(netRes.Content, "POST "+srv.URL+"/lookup") {
		t.Fatalf("network: %+v", netRes)
	}
}

func TestNativeSearchProvidersAndJSBridge(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search/public":
			if r.Header.Get("X-Keenable-Title") == "" {
				http.Error(w, "missing title", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []map[string]any{{"title": "Keenable Hit", "url": "https://example.org/k", "snippet": "Public search works"}},
			})
		case "/res/v1/web/search":
			if r.Header.Get("X-Subscription-Token") != "bkey" {
				http.Error(w, "bad key", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"web": map[string]any{"results": []map[string]any{{"title": "Brave Hit", "url": "https://example.org/b", "description": "Brave result"}}},
			})
		case "/spa":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><div id="root"></div><script>render()</script></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	// Keyless Keenable.
	s1, err := (&Browser{ForceNative: true, AllowLoopback: true, SearchProvider: "keenable", SearchAPIBase: api.URL}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	r1 := s1.Do(context.Background(), Action{Kind: "search", Query: "mesh vpn"})
	if !r1.OK() || !strings.Contains(r1.Content, "Keenable Hit") {
		t.Fatalf("keenable public: %+v", r1)
	}

	// Keyed Brave.
	s2, err := (&Browser{ForceNative: true, AllowLoopback: true, SearchProvider: "brave", SearchAPIKey: "bkey", SearchAPIBase: api.URL}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	r2 := s2.Do(context.Background(), Action{Kind: "search", Query: "mesh vpn"})
	if !r2.OK() || !strings.Contains(r2.Content, "Brave Hit") {
		t.Fatalf("brave: %+v", r2)
	}

	// Headless Chromium/Edge --dump-dom bridge for empty SPA shells.
	if runtime.GOOS != "windows" {
		fakeEdge := filepath.Join(t.TempDir(), "msedge")
		script := "#!/bin/sh\necho '<html><head><title>Rendered SPA</title></head><body><main><h1>Dynamic VPN Dashboard</h1><p>Peer mesh active on 10.88.0.1</p></main></body></html>'\n"
		if err := os.WriteFile(fakeEdge, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		s3, err := (&Browser{ForceNative: true, AllowLoopback: true, JSBrowserPath: fakeEdge}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer s3.Close()
		r3 := s3.Do(context.Background(), Action{Kind: "open", URL: api.URL + "/spa"})
		if !r3.OK() || !strings.Contains(r3.Content, "Peer mesh active on 10.88.0.1") {
			t.Fatalf("SPA JS bridge: %+v", r3)
		}
	}
}
