package lightpanda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultActionTimeout  = 30 * time.Second
	defaultMaxResultBytes = 14_000
	defaultSearchLimit    = 8
	defaultLinksLimit     = 40
	defaultDDGHTMLURL     = "https://html.duckduckgo.com/html/"
	defaultDDGLiteURL     = "https://lite.duckduckgo.com/lite/"
)

// Action is one concrete step Kolibri asks Lightpanda to carry out.
type Action struct {
	Kind      string `json:"action"`
	Query     string `json:"query,omitempty"`
	URL       string `json:"url,omitempty"`
	Selector  string `json:"selector,omitempty"`
	NodeID    int    `json:"node_id,omitempty"`
	Schema    string `json:"schema,omitempty"`
	Value     string `json:"value,omitempty"`
	Key       string `json:"key,omitempty"`
	Submit    bool   `json:"submit,omitempty"`
	Script    string `json:"script,omitempty"`
	WaitUntil string `json:"wait_until,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	MaxDepth  int    `json:"max_depth,omitempty"`
	X         int    `json:"x,omitempty"`
	Y         int    `json:"y,omitempty"`
}

// CanonicalKind normalises synonyms models commonly write for action names.
func CanonicalKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "search", "web_search", "find_web", "query":
		return "search"
	case "open", "goto", "navigate", "visit", "fetch", "read", "markdown", "page", "open_url", "read_page":
		return "open"
	case "html", "raw_html":
		return "html"
	case "links", "list_links", "get_links":
		return "links"
	case "extract", "scrape", "select":
		return "extract"
	case "click", "tap":
		return "click"
	case "fill", "type", "input":
		return "fill"
	case "press", "key", "keypress":
		return "press"
	case "scroll":
		return "scroll"
	case "wait", "wait_for", "waitforselector", "waitforstate", "wait_for_selector", "wait_for_state":
		return "wait"
	case "tree", "dom_tree", "outline":
		return "tree"
	case "elements", "interactive", "interactive_elements", "interactiveelements", "controls", "forms", "detect_forms":
		return "elements"
	case "structured_data", "structureddata", "metadata", "meta":
		return "structured_data"
	case "evaluate", "eval", "js", "javascript", "run_js":
		return "evaluate"
	default:
		return strings.ToLower(strings.TrimSpace(kind))
	}
}

// Describe names the action for the journal and the "Интернет" tab.
func (a Action) Describe() string {
	target := func() string {
		if a.Selector != "" {
			return oneLine(a.Selector, 60)
		}
		if a.NodeID > 0 {
			return fmt.Sprintf("узел #%d", a.NodeID)
		}
		return "страница"
	}
	switch CanonicalKind(a.Kind) {
	case "search":
		return fmt.Sprintf("Поиск: «%s»", oneLine(a.Query, 90))
	case "open":
		if a.Selector != "" {
			return fmt.Sprintf("Страница: %s (%s)", oneLine(a.URL, 90), oneLine(a.Selector, 40))
		}
		return "Страница: " + oneLine(a.URL, 110)
	case "html":
		if a.URL != "" {
			return "HTML: " + oneLine(a.URL, 100)
		}
		return "HTML: " + target()
	case "links":
		if a.URL != "" {
			return "Ссылки: " + oneLine(a.URL, 100)
		}
		return "Ссылки на открытой странице"
	case "extract":
		if a.URL != "" {
			return "Извлечение: " + oneLine(a.URL, 95)
		}
		if a.Selector != "" {
			return "Извлечение: " + oneLine(a.Selector, 70)
		}
		return "Извлечение данных со страницы"
	case "click":
		return "Нажатие: " + target()
	case "fill":
		s := fmt.Sprintf("Ввод в %s: «%s»", target(), oneLine(a.Value, 50))
		if a.Submit {
			s += " + Enter"
		}
		return s
	case "press":
		return "Клавиша: " + oneLine(a.Key, 30)
	case "scroll":
		return fmt.Sprintf("Прокрутка (x=%d, y=%d)", a.X, a.Y)
	case "wait":
		if a.Selector != "" {
			return "Ожидание элемента: " + oneLine(a.Selector, 60)
		}
		if a.WaitUntil != "" {
			return "Ожидание загрузки: " + oneLine(a.WaitUntil, 40)
		}
		return "Ожидание на странице"
	case "tree":
		if a.URL != "" {
			return "Структура страницы: " + oneLine(a.URL, 90)
		}
		return "Структура открытой страницы"
	case "elements":
		if a.URL != "" {
			return "Элементы страницы: " + oneLine(a.URL, 90)
		}
		return "Интерактивные элементы страницы"
	case "structured_data":
		if a.URL != "" {
			return "Метаданные: " + oneLine(a.URL, 95)
		}
		return "Метаданные страницы"
	case "evaluate":
		return "JS на странице: " + oneLine(a.Script, 70)
	default:
		return "Действие «" + oneLine(a.Kind, 40) + "»"
	}
}

// ShortNote is a compact label for the repository work log (.kolibri/worklog.md).
func (a Action) ShortNote() string {
	switch CanonicalKind(a.Kind) {
	case "search":
		return "поиск «" + oneLine(a.Query, 60) + "»"
	case "open", "html", "links", "extract", "tree", "elements", "structured_data":
		if a.URL != "" {
			if u, err := url.Parse(a.URL); err == nil && u.Host != "" {
				p := strings.TrimRight(u.Path, "/")
				return oneLine(u.Host+p, 64)
			}
			return oneLine(a.URL, 64)
		}
	}
	return ""
}

// DedupKey identifies repeatable read requests so an identical lookup inside
// one generation is not executed twice. State-changing actions return "".
func (a Action) DedupKey() string {
	kind := CanonicalKind(a.Kind)
	switch kind {
	case "search":
		q := strings.ToLower(strings.Join(strings.Fields(a.Query), " "))
		if q == "" {
			return ""
		}
		return "search|" + q
	case "open", "html", "links", "extract", "tree", "elements", "structured_data":
		if strings.TrimSpace(a.URL) == "" {
			return "" // reads the current page, which may have changed after a click
		}
		return strings.Join([]string{
			kind,
			strings.TrimSpace(a.URL),
			strings.TrimSpace(a.Selector),
			strings.TrimSpace(a.Schema),
			strings.TrimSpace(a.WaitUntil),
		}, "|")
	}
	return ""
}

// Result is what Lightpanda returned for one Action.
type Result struct {
	Action   Action
	Title    string
	URL      string
	Content  string
	Err      string
	Duration time.Duration
}

// OK reports whether the action succeeded.
func (r Result) OK() bool { return r.Err == "" }

// mcpSession implements Session on top of an mcpConn.
type mcpSession struct {
	conn              mcpConn
	timeout           time.Duration
	maxBytes          int
	allowLoopback     bool
	skipDNS           bool
	searchFallbackURL string
}

func (s *mcpSession) Close() error {
	if s.conn == nil {
		return nil
	}
	return s.conn.close()
}

func (s *mcpSession) Do(ctx context.Context, a Action) Result {
	started := time.Now()
	a.Kind = CanonicalKind(a.Kind)

	actCtx := ctx
	var cancel context.CancelFunc
	if s.timeout > 0 {
		actCtx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	res := Result{Action: a}
	err := s.dispatch(actCtx, a, &res)
	res.Duration = time.Since(started)
	if err != nil {
		if ctx.Err() != nil {
			res.Err = ctx.Err().Error()
		} else if errors.Is(err, context.DeadlineExceeded) {
			res.Err = fmt.Sprintf("превышено время ожидания (%s)", s.timeout.Round(time.Second))
		} else {
			res.Err = err.Error()
		}
		return res
	}
	res.Content = capUTF8(strings.TrimSpace(res.Content), s.maxBytes)
	return res
}

func (s *mcpSession) timeoutMS() int {
	ms := int(s.timeout / time.Millisecond)
	if ms <= 0 {
		return int(defaultActionTimeout / time.Millisecond)
	}
	// Keep Lightpanda's own deadline slightly inside the Go context so it
	// returns a clean timeout message rather than a cancelled call.
	if ms > 2500 {
		return ms - 1500
	}
	return ms
}

func (s *mcpSession) checkTarget(ctx context.Context, raw string) (string, error) {
	u, err := CheckURL(ctx, raw, s.allowLoopback, s.skipDNS)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// guardLandedURL checks where the browser ended up after a navigation, click
// or form submission. If a redirect slipped onto a blocked address, the page
// is cleared to about:blank before anything is read from it.
func (s *mcpSession) guardLandedURL(ctx context.Context, hinted string) (string, error) {
	cur := strings.TrimSpace(hinted)
	if cur == "" {
		if out, err := s.conn.callTool(ctx, "getUrl", map[string]any{}); err == nil && !out.IsError {
			cur = strings.TrimSpace(out.Text)
		}
	}
	if cur == "" || cur == "about:blank" {
		return cur, nil
	}
	// Check literal/hostname safety without an extra DNS round-trip when
	// possible; still check full URL rules.
	if _, err := CheckURL(ctx, cur, s.allowLoopback, s.skipDNS); err != nil {
		_, _ = s.conn.callTool(ctx, "goto", map[string]any{"url": "about:blank"})
		return "", fmt.Errorf("перенаправление заблокировано: %w", err)
	}
	return cur, nil
}

func (s *mcpSession) dispatch(ctx context.Context, a Action, res *Result) error {
	switch a.Kind {
	case "search":
		return s.doSearch(ctx, a, res)
	case "open":
		return s.doOpen(ctx, a, res)
	case "html":
		return s.doHTML(ctx, a, res)
	case "links":
		return s.doLinks(ctx, a, res)
	case "extract":
		return s.doExtract(ctx, a, res)
	case "click":
		return s.doClick(ctx, a, res)
	case "fill":
		return s.doFill(ctx, a, res)
	case "press":
		return s.doPress(ctx, a, res)
	case "scroll":
		return s.doScroll(ctx, a, res)
	case "wait":
		return s.doWait(ctx, a, res)
	case "tree":
		return s.doTree(ctx, a, res)
	case "elements":
		return s.doElements(ctx, a, res)
	case "structured_data":
		return s.doStructuredData(ctx, a, res)
	case "evaluate":
		return s.doEvaluate(ctx, a, res)
	default:
		return fmt.Errorf("неизвестное действие %q (доступны: search, open, links, extract, click, fill, press, scroll, wait, tree, elements, structured_data, evaluate)", a.Kind)
	}
}

// ---- search (native Lightpanda search + browser DuckDuckGo fallback) ----

func (s *mcpSession) doSearch(ctx context.Context, a Action, res *Result) error {
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return errors.New("для search нужно поле query")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 15 {
		limit = 15
	}

	// 1. Lightpanda's built-in search tool (Brave / Tavily / Exa / Parallel /
	// keyless Keenable).
	var nativeErr string
	out, err := s.conn.callTool(ctx, "search", map[string]any{
		"query":   query,
		"timeout": min(s.timeoutMS(), 15_000),
	})
	if err == nil && !out.IsError {
		text := strings.TrimSpace(out.Text)
		if text != "" && !strings.EqualFold(text, "No results.") {
			res.Title = "Результаты поиска: " + query
			res.Content = trimNumberedResults(text, limit)
			return nil
		}
		nativeErr = "ничего не найдено"
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else if err != nil {
		nativeErr = err.Error()
	} else {
		nativeErr = strings.TrimSpace(out.Text)
	}

	// 2. Fallback: open DuckDuckGo inside Lightpanda itself and pull out the
	// organic hits. Works without any API key and survives rate limits on the
	// public search endpoint.
	hits, fbErr := s.duckDuckGoSearch(ctx, query, limit)
	if fbErr == nil && len(hits) > 0 {
		res.Title = "Результаты поиска: " + query
		res.Content = formatSearchHits(hits)
		return nil
	}
	if nativeErr == "ничего не найдено" && fbErr == nil {
		res.Title = "Результаты поиска: " + query
		res.Content = "По запросу «" + query + "» ничего не найдено. Попробуйте переформулировать запрос (например, короче или на английском)."
		return nil
	}
	if fbErr != nil && nativeErr != "" {
		return fmt.Errorf("поиск не удался (%s; резервный поиск: %v)", oneLine(nativeErr, 160), fbErr)
	}
	if fbErr != nil {
		return fbErr
	}
	return errors.New(oneLine(nativeErr, 220))
}

type searchHit struct {
	Title   string
	URL     string
	Snippet string
}

func (s *mcpSession) duckDuckGoSearch(ctx context.Context, query string, limit int) ([]searchHit, error) {
	endpoints := []struct {
		base   string
		schema string
	}{
		{
			base:   defaultDDGHTMLURL,
			schema: `{"results":[{"selector":".result","limit":15,"fields":{"title":".result__a","url":{"selector":".result__a","attr":"href"},"snippet":".result__snippet"}}]}`,
		},
		{
			base:   defaultDDGLiteURL,
			schema: `{"links":[{"selector":"a.result-link","limit":15,"fields":{"title":"","url":{"selector":"","attr":"href"}}}],"snippets":[{"selector":"td.result-snippet","limit":15}]}`,
		},
	}
	if s.searchFallbackURL != "" {
		endpoints = endpoints[:1]
		endpoints[0].base = s.searchFallbackURL
	}

	var lastErr error
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		target := ep.base
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + "q=" + url.QueryEscape(query)
		if _, err := s.checkTarget(ctx, target); err != nil {
			lastErr = err
			continue
		}
		nav, err := s.conn.callTool(ctx, "goto", map[string]any{
			"url":       target,
			"waitUntil": "domcontentloaded",
			"timeout":   min(s.timeoutMS(), 15_000),
		})
		if err != nil {
			lastErr = err
			continue
		}
		if nav.IsError || nav.HTTPStatus >= 400 {
			lastErr = fmt.Errorf("HTTP %d", nav.HTTPStatus)
			continue
		}
		ext, err := s.conn.callTool(ctx, "extract", map[string]any{"schema": ep.schema})
		if err == nil && !ext.IsError {
			if hits := s.parseDDGExtract(ctx, ext.Text, limit); len(hits) > 0 {
				return hits, nil
			}
		}
		// Last-chance extraction via links when page layout differs.
		lnk, err := s.conn.callTool(ctx, "links", map[string]any{"limit": 40})
		if err == nil && !lnk.IsError {
			if hits := s.parseDDGLinks(ctx, lnk.Text, limit); len(hits) > 0 {
				return hits, nil
			}
		}
	}
	return nil, lastErr
}

func (s *mcpSession) parseDDGExtract(ctx context.Context, raw string, limit int) []searchHit {
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
		Links []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"links"`
		Snippets []string `json:"snippets"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return nil
	}
	var out []searchHit
	seen := map[string]bool{}
	add := func(title, rawHref, snippet string) {
		if len(out) >= limit {
			return
		}
		clean := unwrapSearchURL(rawHref)
		if clean == "" || seen[clean] {
			return
		}
		if _, err := CheckURL(ctx, clean, s.allowLoopback, true); err != nil {
			return
		}
		seen[clean] = true
		out = append(out, searchHit{
			Title:   strings.TrimSpace(title),
			URL:     clean,
			Snippet: strings.TrimSpace(snippet),
		})
	}
	for _, r := range payload.Results {
		add(r.Title, r.URL, r.Snippet)
	}
	for i, l := range payload.Links {
		snip := ""
		if i < len(payload.Snippets) {
			snip = payload.Snippets[i]
		}
		add(l.Title, l.URL, snip)
	}
	return out
}

func (s *mcpSession) parseDDGLinks(ctx context.Context, raw string, limit int) []searchHit {
	var items []struct {
		Text string `json:"text"`
		Href string `json:"href"`
	}
	if json.Unmarshal([]byte(raw), &items) != nil {
		return nil
	}
	var out []searchHit
	seen := map[string]bool{}
	for _, it := range items {
		if len(out) >= limit {
			break
		}
		clean := unwrapSearchURL(it.Href)
		if clean == "" || seen[clean] || strings.TrimSpace(it.Text) == "" {
			continue
		}
		if u, err := url.Parse(clean); err != nil || strings.Contains(strings.ToLower(u.Host), "duckduckgo.com") {
			continue
		}
		if _, err := CheckURL(ctx, clean, s.allowLoopback, true); err != nil {
			continue
		}
		seen[clean] = true
		out = append(out, searchHit{Title: strings.TrimSpace(it.Text), URL: clean})
	}
	return out
}

// unwrapSearchURL resolves DuckDuckGo redirect links (//duckduckgo.com/l/?uddg=...)
// and drops paid ad trackers (duckduckgo.com/y.js).
func unwrapSearchURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if strings.HasSuffix(host, "duckduckgo.com") {
		if strings.Contains(u.Path, "/y.js") {
			return "" // sponsored ad link
		}
		if target := u.Query().Get("uddg"); target != "" {
			return strings.TrimSpace(target)
		}
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func formatSearchHits(hits []searchHit) string {
	var b strings.Builder
	for i, h := range hits {
		title := oneLine(h.Title, 160)
		if title == "" {
			title = h.URL
		}
		fmt.Fprintf(&b, "%d. **%s** — %s\n", i+1, title, h.URL)
		if snip := oneLine(h.Snippet, 420); snip != "" {
			fmt.Fprintf(&b, "   %s\n", snip)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// trimNumberedResults caps Lightpanda's "1. **Title** — url" list at limit hits.
func trimNumberedResults(md string, limit int) string {
	if limit <= 0 {
		return md
	}
	marker := fmt.Sprintf("\n%d. **", limit+1)
	if i := strings.Index(md, marker); i >= 0 {
		return strings.TrimSpace(md[:i])
	}
	return md
}

// ---- page reading & interaction -----------------------------------------

func validWaitUntil(w string) string {
	switch strings.ToLower(strings.TrimSpace(w)) {
	case "load", "domcontentloaded", "networkidle", "done":
		return strings.ToLower(strings.TrimSpace(w))
	}
	return ""
}

func (s *mcpSession) navigate(ctx context.Context, rawURL, waitUntil string) (landed, title string, status int, err error) {
	clean, err := s.checkTarget(ctx, rawURL)
	if err != nil {
		return "", "", 0, err
	}
	args := map[string]any{
		"url":     clean,
		"timeout": s.timeoutMS(),
	}
	if wu := validWaitUntil(waitUntil); wu != "" {
		args["waitUntil"] = wu
	}
	out, err := s.conn.callTool(ctx, "goto", args)
	if err != nil {
		return "", "", 0, err
	}
	if out.IsError {
		return "", "", out.HTTPStatus, errors.New(oneLine(out.Text, 240))
	}
	finalURL, err := s.guardLandedURL(ctx, out.PageURL)
	if err != nil {
		return "", "", 0, err
	}
	if finalURL == "" {
		finalURL = clean
	}
	return finalURL, strings.TrimSpace(out.Title), out.HTTPStatus, nil
}

func (s *mcpSession) readCurrentMarkdown(ctx context.Context, selector string, maxBytes int) (string, error) {
	args := map[string]any{
		"maxBytes": maxBytes,
		"timeout":  s.timeoutMS(),
		"strip": map[string]bool{
			"js":        true,
			"css":       true,
			"invisible": true,
			"clutter":   selector == "",
		},
	}
	if selector != "" {
		args["selector"] = selector
	}
	out, err := s.conn.callTool(ctx, "markdown", args)
	if err != nil {
		return "", err
	}
	if out.IsError {
		return "", errors.New(oneLine(out.Text, 240))
	}
	text := strings.TrimSpace(out.Text)
	// Reader-mode stripping occasionally drops a landing page built out of
	// non-article boxes; retry with lighter stripping on the already loaded DOM.
	if selector == "" && len(text) < 120 {
		lighter, lerr := s.conn.callTool(ctx, "markdown", map[string]any{
			"maxBytes": maxBytes,
			"timeout":  s.timeoutMS(),
			"strip":    map[string]bool{"js": true, "css": true, "invisible": true, "shell": true},
		})
		if lerr == nil && !lighter.IsError && len(strings.TrimSpace(lighter.Text)) > len(text) {
			text = strings.TrimSpace(lighter.Text)
		}
	}
	return text, nil
}

func (s *mcpSession) doOpen(ctx context.Context, a Action, res *Result) error {
	var status int
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title, status = u, title, code
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	} else if a.Selector == "" {
		return errors.New("для open нужно поле url")
	}

	md, err := s.readCurrentMarkdown(ctx, strings.TrimSpace(a.Selector), s.maxBytes)
	if err != nil {
		return err
	}
	// Dynamic SPA pages often populate after `load`: wait briefly for network
	// idle once when the initial snapshot came back blank.
	if md == "" && a.Selector == "" && a.WaitUntil == "" {
		_, _ = s.conn.callTool(ctx, "waitForState", map[string]any{"state": "networkidle", "timeout": 4000})
		md, _ = s.readCurrentMarkdown(ctx, "", s.maxBytes)
	}

	var b strings.Builder
	if res.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", res.URL)
	}
	if res.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", res.Title)
	}
	if status > 0 && status != 200 {
		fmt.Fprintf(&b, "HTTP: %d\n", status)
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	if md == "" {
		b.WriteString("(Страница открыта, но видимого текста не нашлось — попробуйте links, elements или wait с wait_until=\"networkidle\".)")
	} else {
		b.WriteString(md)
	}
	res.Content = b.String()
	return nil
}

func (s *mcpSession) doHTML(ctx context.Context, a Action, res *Result) error {
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	args := map[string]any{
		"maxBytes": s.maxBytes,
		"timeout":  s.timeoutMS(),
		"strip":    map[string]bool{"js": true, "css": true, "invisible": true},
	}
	if a.Selector != "" {
		args["selector"] = a.Selector
	} else if a.NodeID > 0 {
		args["backendNodeId"] = a.NodeID
	}
	out, err := s.conn.callTool(ctx, "html", args)
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	res.Content = out.Text
	return nil
}

func (s *mcpSession) doLinks(ctx context.Context, a Action, res *Result) error {
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultLinksLimit
	}
	if limit > 120 {
		limit = 120
	}
	out, err := s.conn.callTool(ctx, "links", map[string]any{
		"limit":   limit,
		"timeout": s.timeoutMS(),
	})
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	// Format JSON link records into a compact, token-light list for the model.
	var items []struct {
		Text          string `json:"text"`
		Href          string `json:"href"`
		BackendNodeID int    `json:"backendNodeId"`
	}
	if json.Unmarshal([]byte(out.Text), &items) == nil {
		if len(items) == 0 {
			res.Content = "(Видимых ссылок на странице нет.)"
			return nil
		}
		var b strings.Builder
		for i, it := range items {
			label := oneLine(it.Text, 100)
			if label == "" {
				label = "(без текста)"
			}
			if it.BackendNodeID > 0 {
				fmt.Fprintf(&b, "%d. [%s](%s) [node_id=%d]\n", i+1, label, it.Href, it.BackendNodeID)
			} else {
				fmt.Fprintf(&b, "%d. [%s](%s)\n", i+1, label, it.Href)
			}
		}
		res.Content = strings.TrimSpace(b.String())
		return nil
	}
	res.Content = out.Text
	return nil
}

func (s *mcpSession) doExtract(ctx context.Context, a Action, res *Result) error {
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	schema := strings.TrimSpace(a.Schema)
	if schema == "" {
		sel := strings.TrimSpace(a.Selector)
		if sel == "" {
			return errors.New("для extract нужно поле schema или selector")
		}
		// Simple selector shortcut: render the matched region as Markdown first,
		// or gather all matching texts when several nodes match.
		quoted, _ := json.Marshal(sel)
		schema = `{"items":[` + string(quoted) + `]}`
	}
	out, err := s.conn.callTool(ctx, "extract", map[string]any{"schema": schema})
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 260))
	}
	res.Content = out.Text
	return nil
}

func targetArgs(a Action) (map[string]any, error) {
	args := map[string]any{}
	if sel := strings.TrimSpace(a.Selector); sel != "" {
		args["selector"] = sel
		return args, nil
	}
	if a.NodeID > 0 {
		args["backendNodeId"] = a.NodeID
		return args, nil
	}
	return nil, errors.New("нужно указать selector или node_id")
}

func (s *mcpSession) snapshotAfterAction(ctx context.Context, out toolOutput, res *Result) error {
	landed, err := s.guardLandedURL(ctx, out.PageURL)
	if err != nil {
		return err
	}
	res.URL = landed
	res.Title = strings.TrimSpace(out.Title)
	var b strings.Builder
	if out.Text != "" {
		b.WriteString(strings.TrimSpace(out.Text))
		b.WriteString("\n")
	}
	if landed != "" && !strings.Contains(out.Text, landed) {
		fmt.Fprintf(&b, "URL: %s\n", landed)
	}
	if res.Title != "" && !strings.Contains(out.Text, res.Title) {
		fmt.Fprintf(&b, "Title: %s\n", res.Title)
	}
	// Include a bounded preview of the updated page so the model sees the
	// effect of a click or form submission without an extra round-trip.
	if md, merr := s.readCurrentMarkdown(ctx, "", min(s.maxBytes, 8000)); merr == nil && md != "" {
		b.WriteString("\n")
		b.WriteString(md)
	}
	res.Content = strings.TrimSpace(b.String())
	return nil
}

func (s *mcpSession) doClick(ctx context.Context, a Action, res *Result) error {
	args, err := targetArgs(a)
	if err != nil {
		return err
	}
	out, err := s.conn.callTool(ctx, "click", args)
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	if wu := validWaitUntil(a.WaitUntil); wu != "" {
		_, _ = s.conn.callTool(ctx, "waitForState", map[string]any{"state": wu, "timeout": 5000})
	}
	return s.snapshotAfterAction(ctx, out, res)
}

func (s *mcpSession) doFill(ctx context.Context, a Action, res *Result) error {
	args, err := targetArgs(a)
	if err != nil {
		return err
	}
	args["value"] = a.Value
	out, err := s.conn.callTool(ctx, "fill", args)
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	if !a.Submit {
		res.URL = out.PageURL
		res.Title = out.Title
		if strings.TrimSpace(out.Text) != "" {
			res.Content = strings.TrimSpace(out.Text)
		} else {
			res.Content = "Поле заполнено."
		}
		return nil
	}
	pressArgs, _ := targetArgs(a)
	pressArgs["key"] = "Enter"
	pout, err := s.conn.callTool(ctx, "press", pressArgs)
	if err != nil {
		return err
	}
	if pout.IsError {
		return errors.New(oneLine(pout.Text, 240))
	}
	wu := validWaitUntil(a.WaitUntil)
	if wu == "" {
		wu = "networkidle"
	}
	_, _ = s.conn.callTool(ctx, "waitForState", map[string]any{"state": wu, "timeout": 5000})
	return s.snapshotAfterAction(ctx, pout, res)
}

func (s *mcpSession) doPress(ctx context.Context, a Action, res *Result) error {
	key := strings.TrimSpace(a.Key)
	if key == "" {
		key = strings.TrimSpace(a.Value)
	}
	if key == "" {
		return errors.New("для press нужно поле key (например \"Enter\")")
	}
	args := map[string]any{"key": key}
	if a.Selector != "" {
		args["selector"] = a.Selector
	} else if a.NodeID > 0 {
		args["backendNodeId"] = a.NodeID
	}
	out, err := s.conn.callTool(ctx, "press", args)
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	if strings.EqualFold(key, "enter") || strings.EqualFold(key, "return") {
		_, _ = s.conn.callTool(ctx, "waitForState", map[string]any{"state": "networkidle", "timeout": 4000})
		return s.snapshotAfterAction(ctx, out, res)
	}
	landed, err := s.guardLandedURL(ctx, out.PageURL)
	if err != nil {
		return err
	}
	res.URL, res.Title = landed, out.Title
	res.Content = strings.TrimSpace(out.Text)
	if res.Content == "" {
		res.Content = "Клавиша " + key + " нажата."
	}
	return nil
}

func (s *mcpSession) doScroll(ctx context.Context, a Action, res *Result) error {
	y := a.Y
	if y == 0 && a.X == 0 {
		y = 1200
	}
	args := map[string]any{"y": y}
	if a.X != 0 {
		args["x"] = a.X
	}
	if a.Selector != "" {
		args["selector"] = a.Selector
	} else if a.NodeID > 0 {
		args["backendNodeId"] = a.NodeID
	}
	out, err := s.conn.callTool(ctx, "scroll", args)
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	return s.snapshotAfterAction(ctx, out, res)
}

func (s *mcpSession) doWait(ctx context.Context, a Action, res *Result) error {
	timeout := min(s.timeoutMS(), 15_000)
	var out toolOutput
	var err error
	switch {
	case strings.TrimSpace(a.Selector) != "":
		out, err = s.conn.callTool(ctx, "waitForSelector", map[string]any{
			"selector": strings.TrimSpace(a.Selector),
			"timeout":  timeout,
		})
	case strings.TrimSpace(a.Script) != "":
		out, err = s.conn.callTool(ctx, "waitForScript", map[string]any{
			"script":  strings.TrimSpace(a.Script),
			"timeout": timeout,
		})
	default:
		state := validWaitUntil(a.WaitUntil)
		if state == "" {
			state = "networkidle"
		}
		out, err = s.conn.callTool(ctx, "waitForState", map[string]any{
			"state":   state,
			"timeout": timeout,
		})
	}
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	res.Content = strings.TrimSpace(out.Text)
	if res.Content == "" {
		res.Content = "Готово."
	}
	return nil
}

func (s *mcpSession) doTree(ctx context.Context, a Action, res *Result) error {
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	args := map[string]any{
		"maxBytes": s.maxBytes,
		"timeout":  s.timeoutMS(),
	}
	if a.NodeID > 0 {
		args["backendNodeId"] = a.NodeID
	}
	if a.MaxDepth > 0 {
		args["maxDepth"] = a.MaxDepth
	}
	out, err := s.conn.callTool(ctx, "tree", args)
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	res.Content = out.Text
	return nil
}

func (s *mcpSession) doElements(ctx context.Context, a Action, res *Result) error {
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	out, err := s.conn.callTool(ctx, "interactiveElements", map[string]any{"timeout": s.timeoutMS()})
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	res.Content = out.Text
	return nil
}

func (s *mcpSession) doStructuredData(ctx context.Context, a Action, res *Result) error {
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	out, err := s.conn.callTool(ctx, "structuredData", map[string]any{"timeout": s.timeoutMS()})
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 240))
	}
	res.Content = out.Text
	return nil
}

func (s *mcpSession) doEvaluate(ctx context.Context, a Action, res *Result) error {
	script := strings.TrimSpace(a.Script)
	if script == "" {
		script = strings.TrimSpace(a.Value)
	}
	if script == "" {
		return errors.New("для evaluate нужно поле script")
	}
	if strings.TrimSpace(a.URL) != "" {
		u, title, code, err := s.navigate(ctx, a.URL, a.WaitUntil)
		if err != nil {
			return err
		}
		res.URL, res.Title = u, title
		if code >= 400 {
			return fmt.Errorf("сервер вернул HTTP %d для %s", code, u)
		}
	}
	out, err := s.conn.callTool(ctx, "evaluate", map[string]any{
		"script":  script,
		"timeout": s.timeoutMS(),
	})
	if err != nil {
		return err
	}
	if out.IsError {
		return errors.New(oneLine(out.Text, 260))
	}
	if _, err := s.guardLandedURL(ctx, out.PageURL); err != nil {
		return err
	}
	res.Content = out.Text
	return nil
}

// capUTF8 cuts s at a valid UTF-8 character boundary no later than maxBytes
// and adds a short note so the model knows more content exists.
func capUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " \t\r\n") +
		"\n\n[…содержимое сокращено до " + strconv.Itoa(maxBytes/1000) + " КБ; используйте selector или extract, чтобы прочитать нужный раздел]"
}
