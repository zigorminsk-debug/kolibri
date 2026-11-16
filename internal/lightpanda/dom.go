package lightpanda

import (
	"html"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

type domNodeKind uint8

const (
	domDocument domNodeKind = iota
	domElement
	domText
)

type domAttr struct {
	Key string // lower-cased attribute name
	Val string
}

// domNode is one node in the built-in browser engine's live DOM tree.
// Every element receives a stable 1-based ID (matching Lightpanda's
// NodeRegistry backendNodeId) so the model can pass node_id across links,
// elements, tree, click, fill, press and html calls.
type domNode struct {
	ID       int
	Kind     domNodeKind
	Tag      string // lower-cased for elements
	Text     string // decoded character data for text nodes
	Attrs    []domAttr
	Parent   *domNode
	Children []*domNode

	// Mutable form state updated by fill.
	hasValue   bool
	value      string
	hasChecked bool
	checked    bool
}

// domDoc holds a parsed HTML document together with its resolved base URL
// and fast node lookup table.
type domDoc struct {
	Root    *domNode
	PageURL *url.URL
	BaseURL *url.URL
	ByID    map[int]*domNode
	nextID  int
}

const (
	maxDOMNodes = 80_000
	maxDOMDepth = 256
)

var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

var rawTextTags = map[string]bool{
	"script": true, "style": true, "xmp": true, "noembed": true, "noframes": true,
}

var rcDataTags = map[string]bool{
	"title": true, "textarea": true,
}

var pClosingTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"details": true, "dialog": true, "div": true, "dl": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hgroup": true, "hr": true, "main": true, "menu": true,
	"nav": true, "ol": true, "p": true, "pre": true, "search": true,
	"section": true, "table": true, "ul": true,
}

var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"body": true, "dd": true, "details": true, "dialog": true, "div": true,
	"dl": true, "dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hgroup": true,
	"hr": true, "html": true, "li": true, "main": true, "menu": true,
	"nav": true, "ol": true, "p": true, "pre": true, "search": true,
	"section": true, "summary": true, "table": true, "tbody": true, "td": true,
	"tfoot": true, "th": true, "thead": true, "tr": true, "ul": true,
}

var metadataTags = map[string]bool{
	"base": true, "head": true, "link": true, "meta": true, "noscript": true,
	"script": true, "style": true, "template": true, "title": true,
}

// parseHTML builds a DOM tree from raw HTML source resolved against rawURL.
func parseHTML(src string, rawURL string) *domDoc {
	var pageURL *url.URL
	if rawURL != "" {
		pageURL, _ = url.Parse(rawURL)
	}
	doc := &domDoc{
		Root:    &domNode{Kind: domDocument},
		PageURL: pageURL,
		BaseURL: pageURL,
		ByID:    make(map[int]*domNode, 256),
	}

	stack := []*domNode{doc.Root}
	nodes := 0
	baseSet := false

	appendNode := func(parent, child *domNode) {
		if nodes >= maxDOMNodes {
			return
		}
		nodes++
		child.Parent = parent
		parent.Children = append(parent.Children, child)
		if child.Kind == domElement {
			doc.nextID++
			child.ID = doc.nextID
			doc.ByID[child.ID] = child
		}
	}

	inScope := func(tag string, stopAt ...string) int {
		for i := len(stack) - 1; i >= 1; i-- {
			t := stack[i].Tag
			if t == tag {
				return i
			}
			for _, stop := range stopAt {
				if t == stop {
					return -1
				}
			}
		}
		return -1
	}

	autoClose := func(tag string) {
		switch {
		case pClosingTags[tag]:
			if idx := inScope("p", "button", "td", "th", "table"); idx > 0 {
				stack = stack[:idx]
			}
		case tag == "li":
			if idx := inScope("li", "ul", "ol", "menu"); idx > 0 {
				stack = stack[:idx]
			}
		case tag == "dt" || tag == "dd":
			for i := len(stack) - 1; i >= 1; i-- {
				if stack[i].Tag == "dl" {
					break
				}
				if stack[i].Tag == "dt" || stack[i].Tag == "dd" {
					stack = stack[:i]
					break
				}
			}
		case tag == "option":
			if len(stack) > 1 && stack[len(stack)-1].Tag == "option" {
				stack = stack[:len(stack)-1]
			}
		case tag == "optgroup":
			if len(stack) > 1 && stack[len(stack)-1].Tag == "option" {
				stack = stack[:len(stack)-1]
			}
			if len(stack) > 1 && stack[len(stack)-1].Tag == "optgroup" {
				stack = stack[:len(stack)-1]
			}
		case tag == "tr":
			if idx := inScope("tr", "table"); idx > 0 {
				stack = stack[:idx]
			}
		case tag == "td" || tag == "th":
			for i := len(stack) - 1; i >= 1; i-- {
				if stack[i].Tag == "tr" || stack[i].Tag == "table" {
					break
				}
				if stack[i].Tag == "td" || stack[i].Tag == "th" {
					stack = stack[:i]
					break
				}
			}
		case tag == "tbody" || tag == "thead" || tag == "tfoot":
			for i := len(stack) - 1; i >= 1; i-- {
				if stack[i].Tag == "table" {
					break
				}
				if stack[i].Tag == "tbody" || stack[i].Tag == "thead" || stack[i].Tag == "tfoot" {
					stack = stack[:i]
					break
				}
			}
		}
	}

	addText := func(raw string, decode bool) {
		if raw == "" || nodes >= maxDOMNodes {
			return
		}
		txt := raw
		if decode && strings.ContainsRune(raw, '&') {
			txt = html.UnescapeString(raw)
		}
		txt = strings.ReplaceAll(txt, "\r\n", "\n")
		txt = strings.ReplaceAll(txt, "\r", "\n")
		parent := stack[len(stack)-1]
		if n := len(parent.Children); n > 0 && parent.Children[n-1].Kind == domText {
			parent.Children[n-1].Text += txt
			return
		}
		appendNode(parent, &domNode{Kind: domText, Text: txt})
	}

	i := 0
	n := len(src)
	for i < n && nodes < maxDOMNodes {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			addText(src[i:], true)
			break
		}
		if lt > 0 {
			addText(src[i:i+lt], true)
			i += lt
		}
		if i+1 >= n {
			addText("<", false)
			break
		}

		// Comments, CDATA, DOCTYPE.
		if src[i+1] == '!' {
			if strings.HasPrefix(src[i:], "<!--") {
				end := strings.Index(src[i+4:], "-->")
				if end < 0 {
					break
				}
				i += 4 + end + 3
				continue
			}
			if len(src)-i >= 9 && strings.EqualFold(src[i:i+9], "<![CDATA[") {
				end := strings.Index(src[i+9:], "]]>")
				if end < 0 {
					addText(src[i+9:], false)
					break
				}
				addText(src[i+9:i+9+end], false)
				i += 9 + end + 3
				continue
			}
			end := strings.IndexByte(src[i+2:], '>')
			if end < 0 {
				break
			}
			i += 2 + end + 1
			continue
		}

		// Processing instructions like <?xml ... ?>.
		if src[i+1] == '?' {
			end := strings.IndexByte(src[i+2:], '>')
			if end < 0 {
				break
			}
			i += 2 + end + 1
			continue
		}

		// Closing tag </tag>.
		if src[i+1] == '/' {
			j := i + 2
			for j < n && isTagNameByte(src[j]) {
				j++
			}
			tag := strings.ToLower(src[i+2 : j])
			end := strings.IndexByte(src[j:], '>')
			if end < 0 {
				break
			}
			i = j + end + 1
			if tag == "" {
				continue
			}
			for k := len(stack) - 1; k >= 1; k-- {
				if stack[k].Tag == tag {
					stack = stack[:k]
					break
				}
				// Don't pop across table boundaries on stray closing tags.
				if stack[k].Tag == "table" && tag != "table" && tag != "body" && tag != "html" {
					break
				}
			}
			continue
		}

		// Start tag <tag attr="val">.
		if !isASCIIAlpha(src[i+1]) {
			addText("<", false)
			i++
			continue
		}
		tag, attrs, selfClose, next := scanStartTag(src, i)
		i = next

		autoClose(tag)
		el := &domNode{Kind: domElement, Tag: tag, Attrs: attrs}
		appendNode(stack[len(stack)-1], el)

		if tag == "base" && !baseSet {
			if href, ok := el.attr("href"); ok && strings.TrimSpace(href) != "" {
				if resolved := doc.resolveURL(href); resolved != "" {
					if u, err := url.Parse(resolved); err == nil {
						doc.BaseURL = u
						baseSet = true
					}
				}
			}
		}

		if voidTags[tag] || selfClose {
			continue
		}

		// Raw-text (<script>, <style>) and RCDATA (<title>, <textarea>) last
		// until their matching closing tag.
		if rawTextTags[tag] || rcDataTags[tag] {
			content, after := scanRawText(src, i, tag)
			if content != "" {
				txt := content
				if rcDataTags[tag] && strings.ContainsRune(txt, '&') {
					txt = html.UnescapeString(txt)
				}
				txt = strings.ReplaceAll(txt, "\r\n", "\n")
				txt = strings.ReplaceAll(txt, "\r", "\n")
				appendNode(el, &domNode{Kind: domText, Text: txt})
			}
			i = after
			continue
		}

		if len(stack) < maxDOMDepth {
			stack = append(stack, el)
		}
	}
	return doc
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isTagNameByte(b byte) bool {
	return isASCIIAlpha(b) || (b >= '0' && b <= '9') || b == '-' || b == ':' || b == '_'
}

func isHTMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func scanStartTag(s string, start int) (tag string, attrs []domAttr, selfClose bool, next int) {
	n := len(s)
	i := start + 1
	for i < n && isTagNameByte(s[i]) {
		i++
	}
	tag = strings.ToLower(s[start+1 : i])

	seen := uint64(0)
	_ = seen
	for i < n {
		for i < n && isHTMLSpace(s[i]) {
			i++
		}
		if i >= n {
			return tag, attrs, false, n
		}
		if s[i] == '>' {
			return tag, attrs, false, i + 1
		}
		if s[i] == '/' {
			if i+1 < n && s[i+1] == '>' {
				// Foreign SVG/MathML elements honor self-closing; HTML elements
				// ignore it unless void, except common XML fragments.
				return tag, attrs, tag == "svg" || tag == "path" || tag == "use" || tag == "math", i + 2
			}
			i++
			continue
		}
		kStart := i
		for i < n && !isHTMLSpace(s[i]) && s[i] != '=' && s[i] != '>' && s[i] != '/' {
			i++
		}
		key := strings.ToLower(s[kStart:i])
		for i < n && isHTMLSpace(s[i]) {
			i++
		}
		val := ""
		if i < n && s[i] == '=' {
			i++
			for i < n && isHTMLSpace(s[i]) {
				i++
			}
			if i < n && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				i++
				vStart := i
				for i < n && s[i] != q {
					i++
				}
				val = s[vStart:i]
				if i < n {
					i++
				}
			} else {
				vStart := i
				for i < n && !isHTMLSpace(s[i]) && s[i] != '>' {
					i++
				}
				val = s[vStart:i]
			}
			if strings.ContainsRune(val, '&') {
				val = html.UnescapeString(val)
			}
		}
		if key != "" && !hasAttrKey(attrs, key) {
			attrs = append(attrs, domAttr{Key: key, Val: val})
		}
	}
	return tag, attrs, false, n
}

func hasAttrKey(attrs []domAttr, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

func scanRawText(s string, start int, tag string) (content string, next int) {
	n := len(s)
	tlen := len(tag)
	for i := start; i < n; i++ {
		if s[i] == '<' && i+2+tlen <= n && s[i+1] == '/' && strings.EqualFold(s[i+2:i+2+tlen], tag) {
			after := i + 2 + tlen
			if after < n && (isHTMLSpace(s[after]) || s[after] == '>' || s[after] == '/') {
				end := strings.IndexByte(s[after:], '>')
				if end < 0 {
					return s[start:i], n
				}
				return s[start:i], after + end + 1
			}
		}
	}
	return s[start:], n
}

// ---- DOM helpers --------------------------------------------------------

func (n *domNode) attr(key string) (string, bool) {
	if n == nil {
		return "", false
	}
	key = strings.ToLower(key)
	for _, a := range n.Attrs {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func (n *domNode) attrVal(key string) string {
	v, _ := n.attr(key)
	return v
}

func (n *domNode) hasAttr(key string) bool {
	_, ok := n.attr(key)
	return ok
}

func (n *domNode) id() string {
	return n.attrVal("id")
}

func (n *domNode) classes() []string {
	c := n.attrVal("class")
	if c == "" {
		return nil
	}
	return strings.Fields(c)
}

func (n *domNode) hasClass(want string) bool {
	c := n.attrVal("class")
	for _, tok := range strings.Fields(c) {
		if tok == want {
			return true
		}
	}
	return false
}

func (n *domNode) textContent() string {
	var b strings.Builder
	var walk func(*domNode)
	walk = func(cur *domNode) {
		if cur == nil {
			return
		}
		if cur.Kind == domText {
			b.WriteString(cur.Text)
			return
		}
		for _, ch := range cur.Children {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

// innerText returns collapsed, human-visible text inside n.
func (n *domNode) innerText() string {
	var b strings.Builder
	var walk func(*domNode)
	walk = func(cur *domNode) {
		if cur == nil {
			return
		}
		if cur.Kind == domText {
			b.WriteString(cur.Text)
			return
		}
		if cur.Kind == domElement {
			if isHiddenElement(cur) {
				return
			}
			if cur.Tag == "br" {
				b.WriteByte('\n')
				return
			}
			block := blockTags[cur.Tag]
			if block && b.Len() > 0 {
				b.WriteByte(' ')
			}
			for _, ch := range cur.Children {
				walk(ch)
			}
			if block {
				b.WriteByte(' ')
			}
			return
		}
		for _, ch := range cur.Children {
			walk(ch)
		}
	}
	walk(n)
	return collapseSpaces(b.String())
}

func (n *domNode) isDisabled() bool {
	for cur := n; cur != nil && cur.Kind == domElement; cur = cur.Parent {
		if cur.hasAttr("disabled") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(cur.attrVal("aria-disabled")), "true") {
			return true
		}
	}
	return false
}

func (n *domNode) controlValue() string {
	if n.hasValue {
		return n.value
	}
	switch n.Tag {
	case "textarea":
		return n.textContent()
	case "select":
		var first string
		var sawFirst bool
		var chosen string
		var walk func(*domNode)
		walk = func(cur *domNode) {
			for _, ch := range cur.Children {
				if ch.Kind != domElement {
					continue
				}
				if ch.Tag == "option" && !ch.hasAttr("disabled") {
					v, ok := ch.attr("value")
					if !ok {
						v = collapseSpaces(ch.textContent())
					}
					if !sawFirst {
						first, sawFirst = v, true
					}
					if ch.hasAttr("selected") {
						chosen = v
					}
				} else {
					walk(ch)
				}
			}
		}
		walk(n)
		if chosen != "" {
			return chosen
		}
		return first
	default:
		return n.attrVal("value")
	}
}

func (n *domNode) isChecked() bool {
	if n.hasChecked {
		return n.checked
	}
	return n.hasAttr("checked")
}

func (n *domNode) closest(tag string) *domNode {
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Kind == domElement && cur.Tag == tag {
			return cur
		}
	}
	return nil
}

func (doc *domDoc) title() string {
	var found string
	var walk func(*domNode) bool
	walk = func(cur *domNode) bool {
		if cur.Kind == domElement {
			if cur.Tag == "svg" {
				return false
			}
			if cur.Tag == "title" {
				found = collapseSpaces(cur.textContent())
				return true
			}
		}
		for _, ch := range cur.Children {
			if walk(ch) {
				return true
			}
		}
		return false
	}
	walk(doc.Root)
	return found
}

// resolveURL resolves a relative or absolute href against the document's base URL.
func (doc *domDoc) resolveURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if doc != nil && doc.BaseURL != nil {
		return doc.BaseURL.ResolveReference(ref).String()
	}
	return ref.String()
}

// markdownAlternateURL returns the document's `<link rel="alternate" type="text/markdown">`
// URL when present and http(s).
func (doc *domDoc) markdownAlternateURL() string {
	for _, el := range doc.queryAll("link[href]") {
		if !hasWordFold(el.attrVal("rel"), "alternate") {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(strings.Split(el.attrVal("type"), ";")[0]))
		if typ != "text/markdown" && typ != "text/x-markdown" {
			continue
		}
		u := doc.resolveURL(el.attrVal("href"))
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			return u
		}
	}
	return ""
}

// metaRefreshURL extracts `<meta http-equiv="refresh" content="0; url=...">`
// when delay is small (<= 5s), used by static documentation redirects.
func (doc *domDoc) metaRefreshURL() string {
	for _, el := range doc.queryAll("meta[content]") {
		if !strings.EqualFold(strings.TrimSpace(el.attrVal("http-equiv")), "refresh") {
			continue
		}
		content := strings.TrimSpace(el.attrVal("content"))
		delayPart, rest, ok := strings.Cut(content, ";")
		if !ok {
			continue
		}
		delay, err := strconv.Atoi(strings.TrimSpace(delayPart))
		if err != nil || delay > 5 {
			continue
		}
		rest = strings.TrimSpace(rest)
		if len(rest) > 4 && strings.EqualFold(rest[:4], "url=") {
			target := strings.Trim(strings.TrimSpace(rest[4:]), `"'`)
			if target != "" {
				return doc.resolveURL(target)
			}
		}
	}
	return ""
}

func hasWordFold(list, want string) bool {
	for _, w := range strings.Fields(list) {
		if strings.EqualFold(w, want) {
			return true
		}
	}
	return false
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSpace := true
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		b.WriteRune(r)
		inSpace = false
	}
	out := b.String()
	return strings.TrimSuffix(out, " ")
}
