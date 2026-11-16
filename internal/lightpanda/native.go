package lightpanda

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// NativeEngineVersion identifies the built-in Go engine built from
	// Lightpanda's tool and reader architecture.
	NativeEngineVersion = "1.0"

	nativeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) KolibriForge/1.0 Lightpanda/1.0 Safari/537.36"
	maxPageBytes    = 8 << 20
)

type netRecord struct {
	Method      string
	URL         string
	Status      int
	ContentType string
	Bytes       int
	Duration    time.Duration
}

type jsBrowser struct {
	Name string
	Path string
}

// nativeSession is a pure-Go, zero-dependency implementation of Lightpanda's
// browsing, reader-mode Markdown, CSS query, form interaction and search tools,
// with optional headless Edge/Chrome DOM rendering for client-side JS SPAs.
// It makes internet research work out of the box on Windows (and on Linux/macOS
// before the standalone Lightpanda binary is installed) with no WSL2 or Docker.
type nativeSession struct {
	client            *http.Client
	timeout           time.Duration
	maxBytes          int
	obeyRobots        bool
	allowLoopback     bool
	skipDNS           bool
	proxy             string
	searchProvider    string
	searchAPIKey      string
	searchFallbackURL string
	searchAPIBase     string
	js                jsBrowser

	// Current page state.
	doc         *domDoc
	pageURL     string
	pageTitle   string
	contentType string
	rawBody     string
	status      int
	lastFocus   *domNode
	netLog      []netRecord
	notes       []string
	robotsCache map[string]robotsRules
}

func newNativeSession(b *Browser) (*nativeSession, error) {
	jar, _ := cookiejar.New(nil)
	s := &nativeSession{
		timeout:           b.actionTimeout(),
		maxBytes:          b.maxResultBytes(),
		obeyRobots:        b.ObeyRobots,
		allowLoopback:     b.AllowLoopback,
		skipDNS:           strings.TrimSpace(b.Proxy) != "",
		proxy:             strings.TrimSpace(b.Proxy),
		searchProvider:    b.SearchProvider,
		searchAPIKey:      b.SearchAPIKey,
		searchFallbackURL: b.SearchFallbackURL,
		searchAPIBase:     b.SearchAPIBase,
		js:                b.findJSBrowser(),
		robotsCache:       map[string]robotsRules{},
	}

	if b.HTTP != nil {
		cp := *b.HTTP
		if cp.Jar == nil {
			cp.Jar = jar
		}
		cp.CheckRedirect = s.checkRedirect
		s.client = &cp
		return s, nil
	}

	dialer := &net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       45 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: s.timeout,
		DisableCompression:    false,
	}
	if s.proxy != "" {
		pu, err := url.Parse(s.proxy)
		if err != nil {
			return nil, fmt.Errorf("неверный HTTP-прокси %q: %w", s.proxy, err)
		}
		tr.Proxy = http.ProxyURL(pu)
		tr.DialContext = dialer.DialContext
	} else {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if s.allowLoopback {
				return dialer.DialContext(ctx, network, addr)
			}
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if addr, ok := netip.AddrFromSlice(ip.IP); ok && blockedAddr(addr.Unmap(), s.allowLoopback) {
					return nil, fmt.Errorf("доступ к локальному или внутреннему адресу %s запрещён", host)
				}
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("не удалось определить IP для %s", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		}
	}

	s.client = &http.Client{
		Transport:     tr,
		Jar:           jar,
		CheckRedirect: s.checkRedirect,
	}
	return s, nil
}

func (s *nativeSession) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("слишком много перенаправлений")
	}
	if _, err := CheckURL(req.Context(), req.URL.String(), s.allowLoopback, s.skipDNS); err != nil {
		return err
	}
	return nil
}

func (s *nativeSession) Close() error {
	if s.client != nil {
		if tr, ok := s.client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	return nil
}

func (s *nativeSession) Do(ctx context.Context, a Action) Result {
	a.Kind = CanonicalKind(a.Kind)
	started := time.Now()
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	res := Result{Action: a}
	var err error
	switch a.Kind {
	case "search":
		err = s.doSearch(c, a, &res)
	case "open":
		err = s.doOpen(c, a, &res)
	case "html":
		err = s.doHTML(c, a, &res)
	case "links":
		err = s.doLinks(c, a, &res)
	case "tree":
		err = s.doTree(c, a, &res)
	case "elements":
		err = s.doElements(c, a, &res)
	case "structured_data":
		err = s.doStructuredData(c, a, &res)
	case "extract":
		err = s.doExtract(c, a, &res)
	case "click":
		err = s.doClick(c, a, &res)
	case "fill":
		err = s.doFill(c, a, &res)
	case "press":
		err = s.doPress(c, a, &res)
	case "scroll":
		res.URL, res.Title, res.Content = s.pageURL, s.pageTitle, "Страница уже загружена целиком."
	case "wait":
		err = s.doWait(c, a, &res)
	case "evaluate":
		err = s.doEvaluate(c, a, &res)
	case "console":
		err = s.doConsole(&res)
	case "network":
		err = s.doNetwork(&res)
	default:
		err = fmt.Errorf("неизвестное действие браузера %q (поддерживаются: search, open, links, elements, tree, structured_data, extract, click, fill, press, wait, html, evaluate, console, network)", a.Kind)
	}
	res.Duration = time.Since(started)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.Content = capUTF8(strings.TrimSpace(res.Content), s.maxBytes)
	return res
}

// ---- navigation & fetching ----------------------------------------------

func (s *nativeSession) fetchRaw(ctx context.Context, method, rawURL string, body io.Reader, contentType string, checkRobots bool) (text, finalURL string, status int, ctype string, err error) {
	u, err := CheckURL(ctx, rawURL, s.allowLoopback, s.skipDNS)
	if err != nil {
		return "", "", 0, "", err
	}
	clean := u.String()
	if checkRobots && s.obeyRobots && method == http.MethodGet {
		if !s.allowedByRobots(ctx, clean) {
			return "", clean, 0, "", fmt.Errorf("доступ к %s запрещён правилами robots.txt", clean)
		}
	}

	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, clean, body)
	if err != nil {
		return "", "", 0, "", err
	}
	req.Header.Set("User-Agent", nativeUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/markdown;q=0.95,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "ru,en-US;q=0.9,en;q=0.8")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.pageURL != "" && method == http.MethodPost {
		req.Header.Set("Referer", s.pageURL)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", 0, "", err
	}
	defer resp.Body.Close()

	finalURL = clean
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if _, err := CheckURL(ctx, finalURL, s.allowLoopback, s.skipDNS); err != nil {
		return "", "", 0, "", err
	}

	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gr, gerr := gzip.NewReader(resp.Body); gerr == nil {
			defer gr.Close()
			reader = gr
		}
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxPageBytes))
	if err != nil {
		return "", finalURL, resp.StatusCode, "", err
	}
	ctype = strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if len(s.netLog) < 200 {
		s.netLog = append(s.netLog, netRecord{
			Method:      method,
			URL:         finalURL,
			Status:      resp.StatusCode,
			ContentType: ctype,
			Bytes:       len(data),
			Duration:    time.Since(started),
		})
	}
	return string(data), finalURL, resp.StatusCode, ctype, nil
}

func (s *nativeSession) loadURL(ctx context.Context, method, rawURL string, body io.Reader, contentType, waitUntil string) error {
	text, finalURL, code, ctype, err := s.fetchRaw(ctx, method, rawURL, body, contentType, true)
	if err != nil {
		return err
	}
	s.pageURL = finalURL
	s.status = code
	s.contentType = ctype
	s.rawBody = text
	s.lastFocus = nil

	if code >= 400 {
		s.doc = parseHTML(text, finalURL)
		s.pageTitle = s.doc.title()
		return fmt.Errorf("сервер вернул HTTP %d для %s", code, finalURL)
	}

	if isHTMLContent(ctype, text) {
		doc := parseHTML(text, finalURL)
		// Follow one immediate <meta http-equiv="refresh"> redirect on GET.
		if method == http.MethodGet {
			if next := doc.metaRefreshURL(); next != "" && next != finalURL {
				if t2, u2, c2, ct2, err2 := s.fetchRaw(ctx, http.MethodGet, next, nil, "", true); err2 == nil && c2 < 400 {
					text, finalURL, code, ctype = t2, u2, c2, ct2
					s.pageURL, s.status, s.contentType, s.rawBody = finalURL, code, ctype, text
					doc = parseHTML(text, finalURL)
				}
			}
		}
		// If the page is a client-side JavaScript SPA shell (or networkidle was
		// explicitly requested) and Edge/Chrome is installed, upgrade the DOM
		// with a headless V8 render.
		if method == http.MethodGet && s.js.Path != "" && (waitUntil == "networkidle" || looksLikeSPAShell(doc)) {
			if rendered, rerr := s.renderWithJSBrowser(ctx, finalURL); rerr == nil && strings.TrimSpace(rendered) != "" {
				rdoc := parseHTML(rendered, finalURL)
				if len(rdoc.Root.innerText()) >= len(doc.Root.innerText()) {
					doc = rdoc
					s.rawBody = rendered
					s.notes = append(s.notes, "JS DOM rendered via "+s.js.Name+" for "+finalURL)
				}
			}
		}
		s.doc = doc
		s.pageTitle = doc.title()
	} else {
		s.doc = nil
		s.pageTitle = ""
	}
	return nil
}

func isHTMLContent(ctype, body string) bool {
	if ctype == "text/html" || ctype == "application/xhtml+xml" {
		return true
	}
	if ctype == "text/plain" || ctype == "text/markdown" || ctype == "text/x-markdown" ||
		strings.Contains(ctype, "json") || strings.Contains(ctype, "csv") {
		return false
	}
	if ctype == "" {
		head := strings.ToLower(strings.TrimSpace(body[:min(len(body), 512)]))
		return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html") || strings.Contains(head, "<body")
	}
	return false
}

func looksLikeSPAShell(doc *domDoc) bool {
	if doc == nil {
		return false
	}
	scripts := len(doc.queryAll("script"))
	if scripts == 0 {
		return false
	}
	visible := len(doc.Root.innerText())
	if visible >= 180 {
		return false
	}
	if len(doc.queryAll("#root, #app, #__next, #__nuxt, app-root")) > 0 {
		return true
	}
	for _, ns := range doc.queryAll("noscript") {
		low := strings.ToLower(ns.textContent())
		if strings.Contains(low, "javascript") || strings.Contains(low, "включите") {
			return true
		}
	}
	return visible < 40 && scripts >= 2
}

func (s *nativeSession) currentMarkdown(ctx context.Context, selector string, maxBytes int) (string, error) {
	if s.doc == nil {
		return formatNonHTMLBody(s.contentType, s.rawBody, maxBytes), nil
	}
	if selector != "" {
		node, err := s.doc.query(selector)
		if err != nil {
			return "", err
		}
		if node == nil {
			return "", fmt.Errorf("элемент %q не найден на странице", selector)
		}
		return s.doc.renderMarkdown(node, renderOpts{maxBytes: maxBytes}), nil
	}

	// Honor <link rel="alternate" type="text/markdown"> when the page offers
	// an author-written Markdown version (same contract as Lightpanda).
	if alt := s.doc.markdownAlternateURL(); alt != "" && s.status == http.StatusOK {
		if body, _, code, ct, err := s.fetchRaw(ctx, http.MethodGet, alt, nil, "", true); err == nil && code == http.StatusOK {
			if ct == "text/markdown" || ct == "text/x-markdown" || ct == "text/plain" {
				if trimmed := strings.TrimSpace(body); trimmed != "" {
					return capUTF8(trimmed, maxBytes), nil
				}
			}
		}
	}

	md := s.doc.renderMarkdown(s.doc.Root, renderOpts{stripShell: true, stripClutter: true, maxBytes: maxBytes})
	if len(md) < 120 {
		if full := s.doc.renderMarkdown(s.doc.Root, renderOpts{maxBytes: maxBytes}); len(full) > len(md) {
			md = full
		}
	}
	if len(md) < 80 {
		if spa := s.doc.embeddedSPAFallback(maxBytes); len(spa) > len(md) {
			md = spa
		}
	}
	return md, nil
}

func formatNonHTMLBody(ctype, raw string, maxBytes int) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(ctype, "json") {
		var buf bytes.Buffer
		if len(trimmed) < 512<<10 && json.Indent(&buf, []byte(trimmed), "", "  ") == nil {
			trimmed = "```json\n" + buf.String() + "\n```"
		}
	} else if ctype != "" && !strings.HasPrefix(ctype, "text/") && !strings.Contains(ctype, "xml") {
		return fmt.Sprintf("(Получен бинарный ответ типа %s, %d байт.)", ctype, len(raw))
	}
	return capUTF8(trimmed, maxBytes)
}

// ---- action handlers ----------------------------------------------------

func (s *nativeSession) doSearch(ctx context.Context, a Action, res *Result) error {
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return errors.New("для search нужно поле query")
	}
	text, err := s.runNativeSearch(ctx, q, a.Limit)
	if err != nil {
		return err
	}
	res.Title = "Поиск: " + q
	res.Content = text
	return nil
}

func (s *nativeSession) doOpen(ctx context.Context, a Action, res *Result) error {
	if u := strings.TrimSpace(a.URL); u != "" {
		if err := s.loadURL(ctx, http.MethodGet, u, nil, "", validWaitUntil(a.WaitUntil)); err != nil {
			res.URL, res.Title = s.pageURL, s.pageTitle
			return err
		}
	} else if s.pageURL == "" {
		return errors.New("для open нужно поле url")
	}
	res.URL = s.pageURL
	res.Title = s.pageTitle

	md, err := s.currentMarkdown(ctx, strings.TrimSpace(a.Selector), s.maxBytes)
	if err != nil {
		return err
	}
	var b strings.Builder
	if res.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", res.URL)
	}
	if res.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", res.Title)
	}
	if s.status > 0 && s.status != http.StatusOK {
		fmt.Fprintf(&b, "HTTP: %d\n", s.status)
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	if md == "" {
		b.WriteString("(Страница открыта, но видимого текста не нашлось — попробуйте links, elements или structured_data.)")
	} else {
		b.WriteString(md)
	}
	res.Content = b.String()
	return nil
}

func (s *nativeSession) ensureOpen(ctx context.Context, a Action, res *Result) error {
	if u := strings.TrimSpace(a.URL); u != "" {
		if err := s.loadURL(ctx, http.MethodGet, u, nil, "", validWaitUntil(a.WaitUntil)); err != nil {
			return err
		}
	}
	if s.doc == nil {
		if s.pageURL == "" {
			return errors.New("сначала откройте страницу действием open")
		}
		return fmt.Errorf("текущий документ (%s) не является HTML-страницей", s.contentType)
	}
	res.URL = s.pageURL
	res.Title = s.pageTitle
	return nil
}

func (s *nativeSession) resolveTarget(a Action, defaultRoot bool) (*domNode, error) {
	if s.doc == nil {
		return nil, errors.New("сначала откройте HTML-страницу действием open")
	}
	if sel := strings.TrimSpace(a.Selector); sel != "" {
		node, err := s.doc.query(sel)
		if err != nil {
			return nil, err
		}
		if node == nil {
			return nil, fmt.Errorf("элемент %q не найден", sel)
		}
		return node, nil
	}
	if a.NodeID > 0 {
		node := s.doc.ByID[a.NodeID]
		if node == nil {
			return nil, fmt.Errorf("элемент с node_id=%d не найден", a.NodeID)
		}
		return node, nil
	}
	if defaultRoot {
		return s.doc.Root, nil
	}
	return nil, errors.New("нужно указать selector или node_id")
}

func (s *nativeSession) doHTML(ctx context.Context, a Action, res *Result) error {
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	node, err := s.resolveTarget(a, true)
	if err != nil {
		return err
	}
	res.Content = s.doc.renderHTML(node, s.maxBytes)
	return nil
}

func (s *nativeSession) doLinks(ctx context.Context, a Action, res *Result) error {
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	limit := a.Limit
	if limit <= 0 || limit > 200 {
		limit = 80
	}
	links := s.doc.collectLinks(limit)
	if len(links) == 0 {
		res.Content = "(На странице не найдено ссылок.)"
		return nil
	}
	var b strings.Builder
	for i, l := range links {
		label := oneLine(l.Text, 120)
		if label == "" {
			label = "(без текста)"
		}
		fmt.Fprintf(&b, "%d. %s — %s [node_id=%d]\n", i+1, label, l.Href, l.NodeID)
	}
	res.Content = b.String()
	return nil
}

func (s *nativeSession) doTree(ctx context.Context, a Action, res *Result) error {
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	node, err := s.resolveTarget(a, true)
	if err != nil {
		return err
	}
	res.Content = s.doc.renderSemanticTree(node, s.maxBytes)
	return nil
}

func (s *nativeSession) doElements(ctx context.Context, a Action, res *Result) error {
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	node, err := s.resolveTarget(a, true)
	if err != nil {
		return err
	}
	res.Content = s.doc.renderInteractive(node, a.Limit, s.maxBytes)
	return nil
}

func (s *nativeSession) doStructuredData(ctx context.Context, a Action, res *Result) error {
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	res.Content = s.doc.renderStructuredData(s.maxBytes)
	return nil
}

func (s *nativeSession) doExtract(ctx context.Context, a Action, res *Result) error {
	sel := strings.TrimSpace(a.Selector)
	if sel == "" {
		return errors.New("для extract нужно поле selector")
	}
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	limit := a.Limit
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	matches, err := s.doc.queryAllErr(sel)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		res.Content = fmt.Sprintf("По селектору %q ничего не найдено.", sel)
		return nil
	}
	var b strings.Builder
	count := 0
	for _, el := range matches {
		txt := el.innerText()
		href := ""
		if el.Tag == "a" && el.hasAttr("href") {
			href = s.doc.resolveURL(el.attrVal("href"))
		}
		if txt == "" && href == "" {
			continue
		}
		count++
		fmt.Fprintf(&b, "%d. %s", count, oneLine(txt, 600))
		if href != "" {
			fmt.Fprintf(&b, " (%s)", href)
		}
		b.WriteByte('\n')
		if count >= limit {
			break
		}
	}
	if count == 0 {
		res.Content = fmt.Sprintf("По селектору %q ничего не найдено.", sel)
		return nil
	}
	res.Content = b.String()
	return nil
}

func (s *nativeSession) doClick(ctx context.Context, a Action, res *Result) error {
	node, err := s.resolveTarget(a, false)
	if err != nil {
		return err
	}
	s.lastFocus = node

	// Clicking inside or on an <a href> navigates to its resolved URL.
	if link := node.closest("a"); link != nil && link.hasAttr("href") {
		href := s.doc.resolveURL(link.attrVal("href"))
		if strings.HasPrefix(href, "#") {
			res.URL, res.Title = s.pageURL, s.pageTitle
			res.Content = "Переход к якорю " + href
			return nil
		}
		if err := s.loadURL(ctx, http.MethodGet, href, nil, "", validWaitUntil(a.WaitUntil)); err != nil {
			return err
		}
		return s.snapshotCurrent(ctx, "Clicked link.", res)
	}

	// Checkbox / radio toggle.
	if node.Tag == "input" {
		t := strings.ToLower(node.attrVal("type"))
		if t == "checkbox" {
			node.hasChecked = true
			node.checked = !node.isChecked()
			res.URL, res.Title = s.pageURL, s.pageTitle
			res.Content = fmt.Sprintf("Флажок переключён: %v.", node.checked)
			return nil
		}
		if t == "radio" {
			if name := node.attrVal("name"); name != "" {
				for _, r := range s.doc.queryAll("input[type=\"radio\" i]") {
					if r.attrVal("name") == name {
						r.hasChecked, r.checked = true, false
					}
				}
			}
			node.hasChecked, node.checked = true, true
			res.URL, res.Title = s.pageURL, s.pageTitle
			res.Content = "Переключатель выбран."
			return nil
		}
	}

	// Submit button inside a <form>.
	if form := node.closest("form"); form != nil && isSubmitControl(node) {
		if err := s.submitForm(ctx, form, node, validWaitUntil(a.WaitUntil)); err != nil {
			return err
		}
		return s.snapshotCurrent(ctx, "Submitted form.", res)
	}

	res.URL, res.Title = s.pageURL, s.pageTitle
	return s.snapshotCurrent(ctx, "Clicked element.", res)
}

func (s *nativeSession) doFill(ctx context.Context, a Action, res *Result) error {
	node, err := s.resolveTarget(a, false)
	if err != nil {
		return err
	}
	if strings.EqualFold(node.attrVal("type"), "password") && !strings.HasPrefix(s.pageURL, "https://") {
		return errors.New("отказ вводить пароль на странице без HTTPS")
	}
	node.hasValue = true
	node.value = a.Value
	s.lastFocus = node

	if !a.Submit {
		res.URL, res.Title = s.pageURL, s.pageTitle
		res.Content = "Поле заполнено."
		return nil
	}
	form := node.closest("form")
	if form == nil {
		return errors.New("поле не находится внутри формы <form>, отправка невозможна")
	}
	if err := s.submitForm(ctx, form, nil, validWaitUntil(a.WaitUntil)); err != nil {
		return err
	}
	return s.snapshotCurrent(ctx, "Submitted form.", res)
}

func (s *nativeSession) doPress(ctx context.Context, a Action, res *Result) error {
	key := strings.TrimSpace(a.Key)
	if key == "" {
		key = strings.TrimSpace(a.Value)
	}
	if key == "" {
		return errors.New("для press нужно поле key (например \"Enter\")")
	}
	target := s.lastFocus
	if strings.TrimSpace(a.Selector) != "" || a.NodeID > 0 {
		n, err := s.resolveTarget(a, false)
		if err != nil {
			return err
		}
		target = n
		s.lastFocus = n
	}
	if strings.EqualFold(key, "Enter") && target != nil {
		if link := target.closest("a"); link != nil && link.hasAttr("href") {
			if err := s.loadURL(ctx, http.MethodGet, s.doc.resolveURL(link.attrVal("href")), nil, "", validWaitUntil(a.WaitUntil)); err != nil {
				return err
			}
			return s.snapshotCurrent(ctx, "Activated link.", res)
		}
		if form := target.closest("form"); form != nil {
			if err := s.submitForm(ctx, form, nil, validWaitUntil(a.WaitUntil)); err != nil {
				return err
			}
			return s.snapshotCurrent(ctx, "Submitted form.", res)
		}
	}
	res.URL, res.Title = s.pageURL, s.pageTitle
	res.Content = fmt.Sprintf("Клавиша %s обработана.", key)
	return nil
}

func (s *nativeSession) doWait(ctx context.Context, a Action, res *Result) error {
	if s.pageURL == "" {
		return errors.New("сначала откройте страницу действием open")
	}
	if sel := strings.TrimSpace(a.Selector); sel != "" && s.doc != nil {
		node, err := s.doc.query(sel)
		if err != nil {
			return err
		}
		if node == nil && s.js.Path != "" {
			if rendered, rerr := s.renderWithJSBrowser(ctx, s.pageURL); rerr == nil && rendered != "" {
				s.doc = parseHTML(rendered, s.pageURL)
				s.pageTitle = s.doc.title()
				node, _ = s.doc.query(sel)
			}
		}
		if node == nil {
			return fmt.Errorf("элемент %q не найден на странице", sel)
		}
	} else if validWaitUntil(a.WaitUntil) == "networkidle" && s.js.Path != "" {
		if rendered, rerr := s.renderWithJSBrowser(ctx, s.pageURL); rerr == nil && rendered != "" {
			s.doc = parseHTML(rendered, s.pageURL)
			s.pageTitle = s.doc.title()
		}
	}
	res.URL, res.Title = s.pageURL, s.pageTitle
	res.Content = fmt.Sprintf("Page ready (%s).", s.pageURL)
	return nil
}

var (
	reQueryAllLen = regexp.MustCompile(`^document\.querySelectorAll\(\s*['"]([^'"]+)['"]\s*\)\.length\s*;?$`)
	reQueryText   = regexp.MustCompile(`^document\.querySelector\(\s*['"]([^'"]+)['"]\s*\)\??\.(textContent|innerText)\s*;?$`)
)

func (s *nativeSession) doEvaluate(ctx context.Context, a Action, res *Result) error {
	script := strings.TrimSpace(a.Script)
	if script == "" {
		script = strings.TrimSpace(a.Value)
	}
	if script == "" {
		return errors.New("для evaluate нужно поле script")
	}
	if err := s.ensureOpen(ctx, a, res); err != nil {
		return err
	}
	switch strings.TrimSuffix(script, ";") {
	case "document.title":
		res.Content = s.pageTitle
		return nil
	case "location.href", "window.location.href", "document.URL":
		res.Content = s.pageURL
		return nil
	case "document.body.innerText", "document.documentElement.innerText":
		res.Content = s.doc.Root.innerText()
		return nil
	}
	if m := reQueryAllLen.FindStringSubmatch(script); m != nil {
		list, err := s.doc.queryAllErr(m[1])
		if err != nil {
			return err
		}
		res.Content = strconv.Itoa(len(list))
		return nil
	}
	if m := reQueryText.FindStringSubmatch(script); m != nil {
		el, err := s.doc.query(m[1])
		if err != nil {
			return err
		}
		if el == nil {
			res.Content = "null"
			return nil
		}
		if m[2] == "textContent" {
			res.Content = strings.TrimSpace(el.textContent())
		} else {
			res.Content = el.innerText()
		}
		return nil
	}
	return errors.New("встроенный движок Lightpanda не выполняет произвольный JS в evaluate — используйте extract с CSS-селектором, open, links, elements или structured_data")
}

func (s *nativeSession) doConsole(res *Result) error {
	res.URL, res.Title = s.pageURL, s.pageTitle
	if len(s.notes) == 0 {
		res.Content = "(Сообщений консоли нет.)"
		return nil
	}
	res.Content = strings.Join(s.notes, "\n")
	return nil
}

func (s *nativeSession) doNetwork(res *Result) error {
	res.URL, res.Title = s.pageURL, s.pageTitle
	if len(s.netLog) == 0 {
		res.Content = "(Сетевых запросов ещё не было.)"
		return nil
	}
	var b strings.Builder
	for i, r := range s.netLog {
		fmt.Fprintf(&b, "%d. %s %s -> HTTP %d (%s, %d B, %dms)\n",
			i+1, r.Method, r.URL, r.Status, r.ContentType, r.Bytes, r.Duration.Milliseconds())
	}
	res.Content = b.String()
	return nil
}

func (s *nativeSession) snapshotCurrent(ctx context.Context, headline string, res *Result) error {
	res.URL = s.pageURL
	res.Title = s.pageTitle
	var b strings.Builder
	b.WriteString(headline)
	b.WriteByte('\n')
	if res.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", res.URL)
	}
	if res.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", res.Title)
	}
	if md, err := s.currentMarkdown(ctx, "", min(s.maxBytes, 8000)); err == nil && md != "" {
		b.WriteByte('\n')
		b.WriteString(md)
	}
	res.Content = strings.TrimSpace(b.String())
	return nil
}

// ---- HTML form submission -----------------------------------------------

func isSubmitControl(el *domNode) bool {
	if el.Tag == "button" {
		t := strings.ToLower(strings.TrimSpace(el.attrVal("type")))
		return t == "" || t == "submit"
	}
	if el.Tag == "input" {
		t := strings.ToLower(strings.TrimSpace(el.attrVal("type")))
		return t == "submit" || t == "image"
	}
	return false
}

func (s *nativeSession) submitForm(ctx context.Context, form, submitter *domNode, waitUntil string) error {
	action := s.pageURL
	if raw := strings.TrimSpace(form.attrVal("action")); raw != "" {
		action = s.doc.resolveURL(raw)
	}
	method := strings.ToUpper(strings.TrimSpace(form.attrVal("method")))
	if method != http.MethodPost {
		method = http.MethodGet
	}
	if submitter != nil {
		if fa := strings.TrimSpace(submitter.attrVal("formaction")); fa != "" {
			action = s.doc.resolveURL(fa)
		}
		if fm := strings.ToUpper(strings.TrimSpace(submitter.attrVal("formmethod"))); fm == http.MethodGet || fm == http.MethodPost {
			method = fm
		}
	}

	vals := url.Values{}
	var collect func(*domNode)
	collect = func(n *domNode) {
		if n.Kind == domElement {
			if n.hasAttr("disabled") {
				return
			}
			name := n.attrVal("name")
			if name != "" {
				switch n.Tag {
				case "input":
					t := strings.ToLower(strings.TrimSpace(n.attrVal("type")))
					switch t {
					case "submit", "button", "reset", "file", "image":
						if n == submitter {
							vals.Add(name, n.controlValue())
						}
					case "checkbox", "radio":
						if n.isChecked() {
							v := n.controlValue()
							if v == "" {
								v = "on"
							}
							vals.Add(name, v)
						}
					default:
						vals.Add(name, n.controlValue())
					}
				case "textarea", "select":
					vals.Add(name, n.controlValue())
				case "button":
					if n == submitter {
						vals.Add(name, n.controlValue())
					}
				}
			}
		}
		for _, ch := range n.Children {
			collect(ch)
		}
	}
	collect(form)

	if method == http.MethodPost {
		encoded := vals.Encode()
		return s.loadURL(ctx, http.MethodPost, action, strings.NewReader(encoded), "application/x-www-form-urlencoded", waitUntil)
	}
	u, err := url.Parse(action)
	if err != nil {
		return err
	}
	u.RawQuery = vals.Encode()
	return s.loadURL(ctx, http.MethodGet, u.String(), nil, "", waitUntil)
}

// ---- robots.txt ---------------------------------------------------------

type robotsRule struct {
	path  string
	allow bool
}

type robotsRules struct {
	rules []robotsRule
}

func (s *nativeSession) allowedByRobots(ctx context.Context, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return true
	}
	origin := u.Scheme + "://" + u.Host
	rules, cached := s.robotsCache[origin]
	if !cached {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		body, _, code, _, ferr := s.fetchRaw(rctx, http.MethodGet, origin+"/robots.txt", nil, "", false)
		cancel()
		if ferr == nil && code == http.StatusOK {
			rules = parseRobots(body)
		}
		s.robotsCache[origin] = rules
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return rules.allows(path)
}

func parseRobots(body string) robotsRules {
	var out robotsRules
	applies := false
	seenDirective := false
	for _, line := range strings.Split(body, "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			applies, seenDirective = false, false
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if seenDirective {
				applies, seenDirective = false, false
			}
			ua := strings.ToLower(v)
			if ua == "*" || strings.Contains(ua, "lightpanda") || strings.Contains(ua, "kolibri") {
				applies = true
			}
		case "disallow":
			seenDirective = true
			if applies && v != "" {
				out.rules = append(out.rules, robotsRule{path: v, allow: false})
			}
		case "allow":
			seenDirective = true
			if applies && v != "" {
				out.rules = append(out.rules, robotsRule{path: v, allow: true})
			}
		default:
			seenDirective = true
		}
	}
	return out
}

func (r robotsRules) allows(path string) bool {
	bestLen := -1
	allowed := true
	for _, rule := range r.rules {
		if robotsMatch(path, rule.path) {
			l := len(rule.path)
			if l > bestLen || (l == bestLen && rule.allow) {
				bestLen = l
				allowed = rule.allow
			}
		}
	}
	return allowed
}

func robotsMatch(path, pattern string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 || (i == 0 && idx != 0) {
			return false
		}
		pos += idx + len(part)
	}
	return !anchored || pos == len(path)
}

// ---- optional headless Edge / Chrome JS DOM bridge ----------------------

// findJSBrowser locates a preinstalled Chromium-based browser (such as
// Microsoft Edge, shipped with every Windows 10/11 system) that can render
// client-side JavaScript SPAs headlessly when the built-in Go engine meets an
// empty SPA shell.
func (b *Browser) findJSBrowser() jsBrowser {
	if b.DisableJSBridge {
		return jsBrowser{}
	}
	if p := strings.TrimSpace(b.JSBrowserPath); p != "" {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return jsBrowser{Name: filepath.Base(p), Path: p}
		}
		return jsBrowser{}
	}
	type cand struct {
		name string
		path string
	}
	var cands []cand
	switch b.goos() {
	case "windows":
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			root := os.Getenv(env)
			if root == "" {
				continue
			}
			cands = append(cands,
				cand{"Microsoft Edge", filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe")},
				cand{"Google Chrome", filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe")},
				cand{"Brave", filepath.Join(root, "BraveSoftware", "Brave-Browser", "Application", "brave.exe")},
			)
		}
		for _, bin := range []struct{ name, exe string }{
			{"Microsoft Edge", "msedge.exe"},
			{"Google Chrome", "chrome.exe"},
			{"Chromium", "chromium.exe"},
		} {
			if p, err := exec.LookPath(bin.exe); err == nil {
				cands = append(cands, cand{bin.name, p})
			}
		}
	case "darwin":
		cands = append(cands,
			cand{"Google Chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
			cand{"Microsoft Edge", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
			cand{"Brave", "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"},
			cand{"Chromium", "/Applications/Chromium.app/Contents/MacOS/Chromium"},
		)
	default:
		for _, bin := range []struct{ name, exe string }{
			{"Chromium", "chromium"},
			{"Chromium", "chromium-browser"},
			{"Google Chrome", "google-chrome"},
			{"Microsoft Edge", "microsoft-edge"},
		} {
			if p, err := exec.LookPath(bin.exe); err == nil {
				cands = append(cands, cand{bin.name, p})
			}
		}
	}
	for _, c := range cands {
		if info, err := os.Stat(c.path); err == nil && !info.IsDir() {
			return jsBrowser{Name: c.name, Path: c.path}
		}
	}
	return jsBrowser{}
}

func (s *nativeSession) renderWithJSBrowser(ctx context.Context, targetURL string) (string, error) {
	if s.js.Path == "" {
		return "", errors.New("headless JS browser not available")
	}
	if _, err := CheckURL(ctx, targetURL, s.allowLoopback, s.skipDNS); err != nil {
		return "", err
	}
	profile, err := os.MkdirTemp("", "kolibri-lp-js-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(profile)

	rctx, cancel := context.WithTimeout(ctx, min(s.timeout, 15*time.Second))
	defer cancel()

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-sync",
		"--metrics-recording-only",
		"--mute-audio",
		"--user-data-dir=" + profile,
		"--virtual-time-budget=4000",
	}
	if s.proxy != "" {
		args = append(args, "--proxy-server="+s.proxy)
	}
	if !s.allowLoopback {
		args = append(args, "--host-resolver-rules=MAP localhost ~NOTFOUND, MAP *.localhost ~NOTFOUND, MAP *.local ~NOTFOUND, MAP 127.0.0.1 ~NOTFOUND, MAP 169.254.169.254 ~NOTFOUND")
	}
	args = append(args, "--dump-dom", targetURL)

	cmd := exec.CommandContext(rctx, s.js.Path, args...)
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, left: maxPageBytes}
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		return "", err
	}
	return out.String(), nil
}

type limitWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left <= 0 {
		return n, nil
	}
	if len(p) > l.left {
		p = p[:l.left]
	}
	l.left -= len(p)
	_, _ = l.w.Write(p)
	return n, nil
}
