package lightpanda

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	defaultBraveURL    = "https://api.search.brave.com"
	defaultTavilyURL   = "https://api.tavily.com"
	defaultExaURL      = "https://api.exa.ai"
	defaultParallelURL = "https://api.parallel.ai"
	defaultKeenableURL = "https://api.keenable.ai"
	keenableAppTitle   = "kolibri-forge"
)

// runNativeSearch executes the Lightpanda search cascade natively in Go:
//  1. When a provider + API key are configured (Brave, Tavily, Exa, Parallel,
//     Keenable), queries that provider directly.
//  2. In keyless auto mode, tries Keenable's public API first and falls back to
//     DuckDuckGo HTML / DuckDuckGo Lite parsed with the built-in DOM engine.
func (s *nativeSession) runNativeSearch(ctx context.Context, query string, limit int) (string, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	prov := strings.ToLower(strings.TrimSpace(s.searchProvider))
	key := strings.TrimSpace(s.searchAPIKey)

	var hits []searchHit
	var err error
	switch {
	case prov == "brave" && key != "":
		hits, err = s.searchBrave(ctx, query, key, limit)
	case prov == "tavily" && key != "":
		hits, err = s.searchTavily(ctx, query, key, limit)
	case prov == "exa" && key != "":
		hits, err = s.searchExa(ctx, query, key, limit)
	case prov == "parallel" && key != "":
		hits, err = s.searchParallel(ctx, query, key, limit)
	case prov == "keenable":
		hits, err = s.searchKeenable(ctx, query, key, limit)
	default:
		// Keyless cascade: if a custom test URL is set, go straight there so
		// tests never touch the public internet.
		if s.searchFallbackURL != "" {
			hits, err = s.searchDuckDuckGoHTML(ctx, query, limit)
		} else {
			var errs []string
			if h, kerr := s.searchKeenable(ctx, query, "", limit); kerr == nil && len(h) > 0 {
				hits = h
			} else if kerr != nil {
				errs = append(errs, kerr.Error())
			}
			if len(hits) == 0 {
				if h, derr := s.searchDuckDuckGoHTML(ctx, query, limit); derr == nil && len(h) > 0 {
					hits = h
				} else if derr != nil {
					errs = append(errs, derr.Error())
				}
			}
			if len(hits) == 0 {
				if h, lerr := s.searchDuckDuckGoLite(ctx, query, limit); lerr == nil && len(h) > 0 {
					hits = h
				} else if lerr != nil {
					errs = append(errs, lerr.Error())
				}
			}
			if len(hits) == 0 && len(errs) > 0 {
				err = errors.New(errs[len(errs)-1])
			}
		}
	}
	if err != nil && len(hits) == 0 {
		return "", fmt.Errorf("поиск не удался: %w", err)
	}
	return formatNativeSearchHits(query, hits, limit), nil
}

func formatNativeSearchHits(query string, hits []searchHit, limit int) string {
	dedup := make([]searchHit, 0, len(hits))
	seen := map[string]bool{}
	for _, h := range hits {
		u := strings.TrimSpace(h.URL)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		dedup = append(dedup, searchHit{Title: h.Title, URL: u, Snippet: h.Snippet})
		if limit > 0 && len(dedup) >= limit {
			break
		}
	}
	if len(dedup) == 0 {
		return fmt.Sprintf("По запросу %q ничего не найдено.", query)
	}
	return fmt.Sprintf("Search results for %q:\n\n%s", query, formatSearchHits(dedup))
}

func (s *nativeSession) searchBrave(ctx context.Context, query, key string, limit int) ([]searchHit, error) {
	base := s.apiBase(defaultBraveURL)
	endpoint := fmt.Sprintf("%s/res/v1/web/search?q=%s&count=%d&text_decorations=false",
		base, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)
	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]searchHit, 0, len(payload.Web.Results))
	for _, r := range payload.Web.Results {
		out = append(out, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

func (s *nativeSession) searchTavily(ctx context.Context, query, key string, limit int) ([]searchHit, error) {
	body := map[string]any{"query": query, "max_results": limit}
	req, err := newJSONPost(ctx, s.apiBase(defaultTavilyURL)+"/search", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]searchHit, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}

func (s *nativeSession) searchExa(ctx context.Context, query, key string, limit int) ([]searchHit, error) {
	body := map[string]any{
		"query":      query,
		"numResults": limit,
		"contents":   map[string]any{"highlights": map[string]any{"numSentences": 3}},
	}
	req, err := newJSONPost(ctx, s.apiBase(defaultExaURL)+"/search", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	var payload struct {
		Results []struct {
			Title      string   `json:"title"`
			URL        string   `json:"url"`
			Highlights []string `json:"highlights"`
			Text       string   `json:"text"`
		} `json:"results"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]searchHit, 0, len(payload.Results))
	for _, r := range payload.Results {
		sn := strings.Join(r.Highlights, " ")
		if sn == "" {
			sn = r.Text
		}
		out = append(out, searchHit{Title: r.Title, URL: r.URL, Snippet: sn})
	}
	return out, nil
}

func (s *nativeSession) searchParallel(ctx context.Context, query, key string, limit int) ([]searchHit, error) {
	body := map[string]any{
		"search_queries": []string{query},
		"mode":           "fast",
		"advanced_settings": map[string]any{
			"max_results":      limit,
			"excerpt_settings": map[string]any{"max_chars_per_result": 500},
		},
	}
	req, err := newJSONPost(ctx, s.apiBase(defaultParallelURL)+"/v1/search", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", key)
	var payload struct {
		Results []struct {
			Title    string   `json:"title"`
			URL      string   `json:"url"`
			Excerpts []string `json:"excerpts"`
		} `json:"results"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]searchHit, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, searchHit{Title: r.Title, URL: r.URL, Snippet: strings.Join(r.Excerpts, " ")})
	}
	return out, nil
}

func (s *nativeSession) searchKeenable(ctx context.Context, query, key string, limit int) ([]searchHit, error) {
	path := "/v1/search/public"
	if key != "" {
		path = "/v1/search"
	}
	body := map[string]any{
		"query":              query,
		"max_results":        limit,
		"snippet_max_length": 500,
	}
	req, err := newJSONPost(ctx, s.apiBase(defaultKeenableURL)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Keenable-Title", keenableAppTitle)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, err
	}
	out := make([]searchHit, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, searchHit{Title: r.Title, URL: r.URL, Snippet: r.Snippet})
	}
	return out, nil
}

func (s *nativeSession) searchDuckDuckGoHTML(ctx context.Context, query string, limit int) ([]searchHit, error) {
	base := s.searchFallbackURL
	if base == "" {
		base = defaultDDGHTMLURL
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	target := base + sep + "q=" + url.QueryEscape(query)
	body, finalURL, code, _, err := s.fetchRaw(ctx, http.MethodGet, target, nil, "", false)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	doc := parseHTML(body, finalURL)
	return extractDDGHits(doc, limit), nil
}

func (s *nativeSession) searchDuckDuckGoLite(ctx context.Context, query string, limit int) ([]searchHit, error) {
	form := url.Values{"q": {query}}.Encode()
	body, finalURL, code, _, err := s.fetchRaw(ctx, http.MethodPost, defaultDDGLiteURL,
		strings.NewReader(form), "application/x-www-form-urlencoded", false)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	doc := parseHTML(body, finalURL)
	return extractDDGHits(doc, limit), nil
}

func extractDDGHits(doc *domDoc, limit int) []searchHit {
	var hits []searchHit
	// Standard DuckDuckGo HTML markup (.result / a.result__a / .result__snippet).
	for _, card := range doc.queryAll(".result, .web-result, .results_links") {
		if card.hasClass("result--ad") {
			continue
		}
		a, _ := (&domDoc{Root: card, BaseURL: doc.BaseURL}).query("a.result__a, h2 a, a[href]")
		if a == nil {
			continue
		}
		href := unwrapSearchURL(doc.resolveURL(a.attrVal("href")))
		if !isPublicSearchHit(href) {
			continue
		}
		title := collapseSpaces(a.innerText())
		var snippet string
		if sn, _ := (&domDoc{Root: card, BaseURL: doc.BaseURL}).query(".result__snippet, .snippet"); sn != nil {
			snippet = collapseSpaces(sn.innerText())
		}
		hits = append(hits, searchHit{Title: title, URL: href, Snippet: snippet})
		if limit > 0 && len(hits) >= limit {
			return hits
		}
	}
	if len(hits) > 0 {
		return hits
	}
	// DuckDuckGo Lite table layout (a.result-link + td.result-snippet).
	links := doc.queryAll("a.result-link")
	snippets := doc.queryAll("td.result-snippet")
	for i, a := range links {
		href := unwrapSearchURL(doc.resolveURL(a.attrVal("href")))
		if !isPublicSearchHit(href) {
			continue
		}
		sn := ""
		if i < len(snippets) {
			sn = collapseSpaces(snippets[i].innerText())
		}
		hits = append(hits, searchHit{
			Title:   collapseSpaces(a.innerText()),
			URL:     href,
			Snippet: sn,
		})
		if limit > 0 && len(hits) >= limit {
			break
		}
	}
	return hits
}

func isPublicSearchHit(u string) bool {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return false
	}
	return !strings.Contains(u, "duckduckgo.com/y.js") && !strings.Contains(u, "duckduckgo.com/lite")
}

func (s *nativeSession) apiBase(def string) string {
	if s.searchAPIBase != "" {
		return strings.TrimRight(s.searchAPIBase, "/")
	}
	return def
}

func newJSONPost(ctx context.Context, endpoint string, body any) (*http.Request, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (s *nativeSession) doJSON(req *http.Request, dst any) error {
	req.Header.Set("User-Agent", nativeUserAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, oneLine(string(data), 180))
	}
	return json.Unmarshal(data, dst)
}
