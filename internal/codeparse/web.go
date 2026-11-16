package codeparse

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/zigorminsk-debug/kolibri/internal/lightpanda"
)

var (
	webMarker       = regexp.MustCompile(`(?i)^\s*={2,}\s*(?:WEB|BROWSER|LIGHTPANDA)\s*={2,}\s*$`)
	webInlineMarker = regexp.MustCompile(`(?i)^\s*={2,}\s*(WEB|BROWSER|LIGHTPANDA|SEARCH|OPEN|BROWSE|GOTO)\s*:\s*(.+?)\s*={2,}\s*$`)
	notesMarker     = regexp.MustCompile(`(?i)^\s*={2,}\s*NOTES?\s*={2,}\s*$`)
	endWebMarker    = regexp.MustCompile(`(?i)^\s*={2,}\s*END(?:\s+OF)?(?:\s+(?:WEB|BROWSER|LIGHTPANDA))?\s*={2,}\s*$`)
	endNotesMarker  = regexp.MustCompile(`(?i)^\s*={2,}\s*END(?:\s+OF)?(?:\s+NOTES?)?\s*={2,}\s*$`)
	listPrefix      = regexp.MustCompile(`^\s*(?:[-*•+]\s+|\d+[.)]\s+)`)
	shorthandLine   = regexp.MustCompile(`(?i)^([a-zа-яё_]+)\s*(?:[:：]\s*|\s+)(.+)$`)
)

func parseInlineWeb(kind, rest string) []lightpanda.Action {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	switch strings.ToUpper(kind) {
	case "SEARCH":
		q := strings.Trim(rest, "`\"' ")
		if q == "" {
			return nil
		}
		return []lightpanda.Action{{Kind: "search", Query: q}}
	case "OPEN", "BROWSE", "GOTO":
		u := strings.Trim(rest, "`\"'<> ")
		if u == "" {
			return nil
		}
		return []lightpanda.Action{{Kind: "open", URL: u}}
	default:
		return parseWebBlock([]string{rest})
	}
}

// parseWebBlock turns the lines of a === WEB === section into concrete
// Lightpanda actions. It accepts JSON Lines (the requested format), a JSON
// array, multi-line JSON objects, Markdown-fenced JSON, and simple
// "search: ..." / "open: https://..." lines when a model drifts.
func parseWebBlock(lines []string) []lightpanda.Action {
	// Drop blank lines, Markdown fences and comment lines.
	var body []string
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" || fenceLine.MatchString(t) {
			continue
		}
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") {
			continue
		}
		body = append(body, t)
	}
	if len(body) == 0 {
		return nil
	}

	joined := strings.Join(body, "\n")
	if acts, ok := decodeJSONStream(joined); ok && len(acts) > 0 {
		return acts
	}

	// Mixed or shorthand block: read multi-line JSON objects balanced by braces
	// and fall back to line-by-line parsing for everything else.
	var out []lightpanda.Action
	for i := 0; i < len(body); i++ {
		line := strings.TrimSpace(listPrefix.ReplaceAllString(body[i], ""))
		line = strings.TrimSuffix(line, ",")
		if line == "" || line == "[" || line == "]" {
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			chunk, end := gatherJSONChunk(body, i)
			if acts, ok := decodeJSONStream(chunk); ok && len(acts) > 0 {
				out = append(out, acts...)
				i = end
				continue
			}
			// An unfinished trailing JSON fragment (cut-off reply) is ignored.
			if end == len(body)-1 {
				break
			}
		}
		if a, ok := parseShorthandAction(line); ok {
			out = append(out, a)
		}
	}
	return out
}

// gatherJSONChunk joins lines from start until braces/brackets balance.
func gatherJSONChunk(lines []string, start int) (string, int) {
	var b strings.Builder
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if i == start {
			l = strings.TrimSpace(listPrefix.ReplaceAllString(l, ""))
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
		for j := 0; j < len(l); j++ {
			c := l[j]
			if inStr {
				if esc {
					esc = false
				} else if c == '\\' {
					esc = true
				} else if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth <= 0 && !inStr {
			s := strings.TrimSuffix(strings.TrimSpace(b.String()), ",")
			return s, i
		}
	}
	return strings.TrimSuffix(strings.TrimSpace(b.String()), ","), len(lines) - 1
}

// decodeJSONStream parses one or more concatenated JSON objects or arrays.
func decodeJSONStream(src string) ([]lightpanda.Action, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(src)))
	dec.UseNumber()
	var out []lightpanda.Action
	for dec.More() {
		var v any
		if err := dec.Decode(&v); err != nil {
			return out, false
		}
		out = append(out, actionsFromValue(v)...)
	}
	return out, true
}

func actionsFromValue(v any) []lightpanda.Action {
	switch x := v.(type) {
	case []any:
		var out []lightpanda.Action
		for _, item := range x {
			out = append(out, actionsFromValue(item)...)
		}
		return out
	case map[string]any:
		// Unwrap {"actions":[...]} or {"requests":[...]} wrappers.
		for _, k := range []string{"actions", "requests", "steps", "web"} {
			if sub, ok := x[k].([]any); ok {
				return actionsFromValue(sub)
			}
		}
		if a, ok := actionFromMap(x); ok {
			return []lightpanda.Action{a}
		}
	case string:
		if a, ok := parseShorthandAction(x); ok {
			return []lightpanda.Action{a}
		}
	}
	return nil
}

func actionFromMap(m map[string]any) (lightpanda.Action, bool) {
	// Case-insensitive lookup with snake_case normalisation.
	norm := make(map[string]any, len(m))
	for k, v := range m {
		nk := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), "-", "_"))
		norm[nk] = v
	}
	// Flatten nested {"arguments":{...}} or {"params":{...}} if a model wrote
	// an MCP-style tool call object.
	for _, sub := range []string{"arguments", "args", "params"} {
		if inner, ok := norm[sub].(map[string]any); ok {
			for k, v := range inner {
				nk := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), "-", "_"))
				if _, exists := norm[nk]; !exists {
					norm[nk] = v
				}
			}
		}
	}

	str := func(keys ...string) string {
		for _, k := range keys {
			v, ok := norm[k]
			if !ok || v == nil {
				continue
			}
			switch s := v.(type) {
			case string:
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			case json.Number:
				return s.String()
			}
		}
		return ""
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			v, ok := norm[k]
			if !ok || v == nil {
				continue
			}
			switch n := v.(type) {
			case json.Number:
				if i, err := n.Int64(); err == nil {
					return int(i)
				}
				if f, err := n.Float64(); err == nil {
					return int(f)
				}
			case float64:
				return int(n)
			case string:
				if i, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(n), "#")); err == nil {
					return i
				}
			}
		}
		return 0
	}
	flag := func(keys ...string) bool {
		for _, k := range keys {
			v, ok := norm[k]
			if !ok || v == nil {
				continue
			}
			switch b := v.(type) {
			case bool:
				return b
			case string:
				switch strings.ToLower(strings.TrimSpace(b)) {
				case "true", "1", "yes", "да":
					return true
				}
			}
		}
		return false
	}

	var a lightpanda.Action
	rawKind := str("action", "type", "tool", "name", "cmd", "command", "op", "method")
	a.Kind = lightpanda.CanonicalKind(rawKind)
	a.Query = str("query", "q", "search", "keywords")
	a.URL = str("url", "href", "link", "uri")
	a.Selector = str("selector", "css", "css_selector", "target", "element")
	a.NodeID = num("node_id", "nodeid", "backend_node_id", "backendnodeid")
	a.Value = str("value", "input", "content")
	a.Key = str("key")
	a.Submit = flag("submit", "enter", "press_enter")
	a.Script = str("script", "code", "expression", "js", "javascript")
	a.WaitUntil = str("wait_until", "waituntil", "state")
	a.Limit = num("limit", "count", "top_k", "max_results")
	a.MaxDepth = num("max_depth", "maxdepth", "depth")
	a.X = num("x", "dx", "scroll_x")
	a.Y = num("y", "dy", "scroll_y", "amount", "pixels")

	// Schema may arrive as a JSON string or an inline JSON object.
	if raw, ok := norm["schema"]; ok && raw != nil {
		switch s := raw.(type) {
		case string:
			a.Schema = strings.TrimSpace(s)
		default:
			if enc, err := json.Marshal(s); err == nil {
				a.Schema = string(enc)
			}
		}
	}

	// Disambiguate generic "text" according to the action.
	if txt := str("text"); txt != "" {
		switch a.Kind {
		case "search":
			if a.Query == "" {
				a.Query = txt
			}
		case "fill":
			if a.Value == "" {
				a.Value = txt
			}
		case "":
			if a.URL == "" && a.Query == "" {
				a.Query = txt
			}
		}
	}

	// Friendly scroll direction helper when a model writes {"action":"scroll","direction":"down"}.
	if a.Kind == "scroll" && a.X == 0 && a.Y == 0 {
		applyScrollDirection(&a, str("direction", "dir"))
	}

	// Infer obvious actions when the "action" field was omitted.
	if a.Kind == "" {
		switch {
		case a.Query != "" && a.URL == "":
			a.Kind = "search"
		case a.Schema != "":
			a.Kind = "extract"
		case a.Script != "":
			a.Kind = "evaluate"
		case a.Value != "" && (a.Selector != "" || a.NodeID > 0):
			a.Kind = "fill"
		case a.URL != "":
			a.Kind = "open"
		default:
			return a, false
		}
	}

	return a, validAction(a)
}

func parseShorthandAction(line string) (lightpanda.Action, bool) {
	line = strings.TrimSpace(strings.Trim(line, "`"))
	if line == "" {
		return lightpanda.Action{}, false
	}
	if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
		return lightpanda.Action{Kind: "open", URL: strings.Fields(line)[0]}, true
	}
	switch k := lightpanda.CanonicalKind(line); k {
	case "links", "tree", "elements", "structured_data", "html":
		return lightpanda.Action{Kind: k}, true
	}

	m := shorthandLine.FindStringSubmatch(line)
	if m == nil {
		return lightpanda.Action{}, false
	}
	verb := strings.ToLower(m[1])
	rest := strings.TrimSpace(m[2])
	unquote := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
			return strings.TrimSpace(s[1 : len(s)-1])
		}
		return s
	}

	var kind string
	switch verb {
	case "поиск", "найти", "ищи":
		kind = "search"
	case "открой", "открыть", "страница":
		kind = "open"
	case "ссылки":
		kind = "links"
	default:
		kind = lightpanda.CanonicalKind(verb)
	}

	var a lightpanda.Action
	a.Kind = kind
	switch kind {
	case "search":
		a.Query = unquote(rest)
	case "open", "links", "tree", "elements", "structured_data":
		u := strings.Trim(unquote(rest), "<>")
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			a.URL = u
		} else if kind == "open" {
			return a, false
		}
	case "html":
		u := unquote(rest)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			a.URL = u
		} else {
			a.Selector = u
		}
	case "extract":
		if left, right, ok := strings.Cut(rest, "|"); ok {
			l, r := strings.TrimSpace(left), strings.TrimSpace(right)
			if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
				a.URL, a.Selector = l, r
			} else {
				a.Selector = rest
			}
		} else if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			a.URL = rest
		} else {
			a.Selector = unquote(rest)
		}
	case "click":
		t := unquote(rest)
		if id, err := strconv.Atoi(strings.TrimPrefix(t, "#")); err == nil && strings.HasPrefix(t, "#") {
			a.NodeID = id
		} else {
			a.Selector = t
		}
	case "fill":
		if left, right, ok := strings.Cut(rest, "="); ok {
			a.Selector = strings.TrimSpace(left)
			a.Value = unquote(right)
		}
	case "press":
		a.Key = unquote(rest)
	case "scroll":
		fields := strings.Fields(rest)
		if len(fields) == 2 {
			x, ex := strconv.Atoi(fields[0])
			y, ey := strconv.Atoi(fields[1])
			if ex == nil && ey == nil {
				a.X, a.Y = x, y
			}
		} else if y, err := strconv.Atoi(rest); err == nil {
			a.Y = y
		} else {
			applyScrollDirection(&a, rest)
		}
	case "wait":
		a.Selector = unquote(rest)
	case "evaluate":
		a.Script = rest
	default:
		return a, false
	}
	return a, validAction(a)
}

func applyScrollDirection(a *lightpanda.Action, dir string) {
	switch strings.ToLower(strings.TrimSpace(dir)) {
	case "down", "вниз":
		a.Y = 800
	case "up", "вверх":
		a.Y = -800
	case "bottom", "end":
		a.Y = 4000
	case "top", "home":
		a.Y = -4000
	case "right":
		a.X = 600
	case "left":
		a.X = -600
	}
}

func validAction(a lightpanda.Action) bool {
	switch lightpanda.CanonicalKind(a.Kind) {
	case "search":
		return strings.TrimSpace(a.Query) != ""
	case "open":
		return strings.TrimSpace(a.URL) != ""
	case "click":
		return a.Selector != "" || a.NodeID > 0
	case "fill":
		return a.Selector != "" || a.NodeID > 0
	case "press":
		return strings.TrimSpace(a.Key) != ""
	case "evaluate":
		return strings.TrimSpace(a.Script) != ""
	case "html", "links", "extract", "scroll", "wait", "tree", "elements", "structured_data":
		return true
	default:
		return false
	}
}
