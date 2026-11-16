package lightpanda

import (
	"encoding/json"
	"fmt"
	"html"
	"strconv"
	"strings"
)

type renderOpts struct {
	stripShell   bool
	stripClutter bool
	maxBytes     int
}

// renderMarkdown converts root (or the main content container when
// stripClutter is on) into compact, readable CommonMark.
func (doc *domDoc) renderMarkdown(root *domNode, opts renderOpts) string {
	if doc == nil || root == nil {
		return ""
	}
	target := root
	stripShell := opts.stripShell || opts.stripClutter

	if opts.stripClutter && root == doc.Root {
		if main := pickMainContent(doc); main != nil {
			target = main
			stripShell = true
		}
	}
	if stripShell && shouldUndoShellStrip(target) {
		stripShell = false
	}

	r := &mdRenderer{
		doc:        doc,
		stripShell: stripShell,
		atLine:     true,
	}
	r.walk(target)
	out := strings.TrimSpace(collapseBlankLines(r.b.String()))
	if opts.maxBytes > 0 && len(out) > opts.maxBytes {
		return capUTF8(out, opts.maxBytes)
	}
	return out
}

// renderHTML serializes root with script, style and hidden nodes stripped.
func (doc *domDoc) renderHTML(root *domNode, maxBytes int) string {
	if doc == nil || root == nil {
		return ""
	}
	var b strings.Builder
	var write func(*domNode)
	write = func(n *domNode) {
		if maxBytes > 0 && b.Len() > maxBytes+256 {
			return
		}
		switch n.Kind {
		case domDocument:
			for _, ch := range n.Children {
				write(ch)
			}
		case domText:
			b.WriteString(html.EscapeString(n.Text))
		case domElement:
			if n != root && isHiddenElement(n) {
				return
			}
			b.WriteByte('<')
			b.WriteString(n.Tag)
			for _, a := range n.Attrs {
				if strings.HasPrefix(a.Key, "on") || a.Key == "style" {
					continue
				}
				b.WriteByte(' ')
				b.WriteString(a.Key)
				if a.Val != "" {
					b.WriteString(`="`)
					b.WriteString(html.EscapeString(a.Val))
					b.WriteByte('"')
				}
			}
			b.WriteByte('>')
			if voidTags[n.Tag] {
				return
			}
			for _, ch := range n.Children {
				write(ch)
			}
			b.WriteString("</")
			b.WriteString(n.Tag)
			b.WriteByte('>')
		}
	}
	write(root)
	return capUTF8(strings.TrimSpace(b.String()), maxBytes)
}

// ---- visibility & clutter filters ---------------------------------------

func isHiddenElement(el *domNode) bool {
	if el == nil || el.Kind != domElement {
		return false
	}
	if metadataTags[el.Tag] || el.Tag == "svg" {
		return true
	}
	if el.hasAttr("hidden") {
		return true
	}
	if el.Tag == "input" && strings.EqualFold(el.attrVal("type"), "hidden") {
		return true
	}
	if styleHides(el.attrVal("style")) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(el.attrVal("aria-hidden")), "true") {
		// Keep block-holding app roots hidden behind a modal; skip inline icons.
		if !blockTags[el.Tag] && !containsBlock(el) {
			return true
		}
	}
	return false
}

func styleHides(style string) bool {
	if style == "" {
		return false
	}
	for _, decl := range strings.Split(style, ";") {
		prop, val, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		val = strings.ToLower(strings.TrimSpace(val))
		if idx := strings.Index(val, "!important"); idx >= 0 {
			val = strings.TrimSpace(val[:idx])
		}
		if prop == "display" && val == "none" {
			return true
		}
		if prop == "visibility" && (val == "hidden" || val == "collapse") {
			return true
		}
	}
	return false
}

func containsBlock(el *domNode) bool {
	for _, ch := range el.Children {
		if ch.Kind != domElement || metadataTags[ch.Tag] || ch.Tag == "svg" || styleHides(ch.attrVal("style")) {
			continue
		}
		if blockTags[ch.Tag] || containsBlock(ch) {
			return true
		}
	}
	return false
}

var chromeRoles = map[string]bool{
	"banner": true, "complementary": true, "contentinfo": true,
	"navigation": true, "search": true, "dialog": true,
	"alertdialog": true, "menu": true, "menubar": true,
}

var chromeWords = map[string]bool{
	"header": true, "footer": true, "nav": true, "navbar": true,
	"navigation": true, "sidebar": true, "masthead": true,
	"cookie-banner": true, "cookiebanner": true, "breadcrumbs": true,
}

func isChromeElement(el *domNode) bool {
	switch el.Tag {
	case "nav", "aside", "dialog":
		return true
	case "header", "footer":
		return !insideSectioning(el)
	}
	if r := strings.ToLower(strings.TrimSpace(el.attrVal("role"))); chromeRoles[r] {
		return true
	}
	switch el.Tag {
	case "div", "section", "ul", "ol", "form", "table", "td", "p":
		if !insideSectioning(el) && (hasChromeWord(el.attrVal("class")) || hasChromeWord(el.attrVal("id"))) {
			return true
		}
	}
	return false
}

func insideSectioning(el *domNode) bool {
	for p := el.Parent; p != nil; p = p.Parent {
		if p.Kind != domElement {
			continue
		}
		switch p.Tag {
		case "article", "main", "section", "aside", "nav":
			return true
		}
	}
	return false
}

func hasChromeWord(s string) bool {
	if s == "" {
		return false
	}
	for _, tok := range strings.Fields(s) {
		if chromeWords[strings.ToLower(tok)] {
			return true
		}
	}
	return false
}

// shouldUndoShellStrip keeps page chrome when removing it would leave less
// than a quarter of the page's non-link prose.
func shouldUndoShellStrip(root *domNode) bool {
	var all, prose, shellAll, shellProse int
	var measure func(n *domNode, inShell, inLink bool)
	measure = func(n *domNode, inShell, inLink bool) {
		if n.Kind == domText {
			cnt := nonSpaceCount(n.Text)
			all += cnt
			if inShell {
				shellAll += cnt
			}
			if !inLink {
				prose += cnt
				if inShell {
					shellProse += cnt
				}
			}
			return
		}
		if n.Kind == domElement {
			if isHiddenElement(n) {
				return
			}
			if isChromeElement(n) {
				inShell = true
			}
			if n.Tag == "a" && n.hasAttr("href") {
				inLink = true
			}
		}
		for _, ch := range n.Children {
			measure(ch, inShell, inLink)
		}
	}
	measure(root, false, false)
	total, shell := prose, shellProse
	if total == 0 {
		total, shell = all, shellAll
	}
	if total == 0 {
		return false
	}
	return (total-shell)*4 < total
}

func pickMainContent(doc *domDoc) *domNode {
	totalProse := proseCount(doc.Root, true)
	if totalProse < 500 {
		return nil
	}
	for _, sel := range []string{"main", "[role=\"main\" i]", "article"} {
		matches := doc.queryAll(sel)
		if sel == "article" && len(matches) != 1 {
			// Listing pages have many <article> cards; keep them all.
			continue
		}
		for _, m := range matches {
			if isHiddenElement(m) {
				continue
			}
			mp := proseCount(m, true)
			if mp >= 400 && mp*10 >= totalProse*4 {
				return m
			}
		}
	}
	return nil
}

func proseCount(root *domNode, skipShell bool) int {
	total := 0
	var walk func(n *domNode, inLink bool)
	walk = func(n *domNode, inLink bool) {
		if n.Kind == domText {
			if !inLink {
				total += nonSpaceCount(n.Text)
			}
			return
		}
		if n.Kind == domElement {
			if isHiddenElement(n) || (skipShell && isChromeElement(n)) {
				return
			}
			if n.Tag == "a" && n.hasAttr("href") {
				inLink = true
			}
		}
		for _, ch := range n.Children {
			walk(ch, inLink)
		}
	}
	walk(root, false)
	return total
}

func nonSpaceCount(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if !isHTMLSpace(s[i]) {
			n++
		}
	}
	return n
}

// ---- CommonMark serializer ----------------------------------------------

type mdRenderer struct {
	doc        *domDoc
	stripShell bool
	b          strings.Builder
	atLine     bool
	inPre      bool
	inCode     bool
	inTable    bool
	inLink     bool
	listDepth  int
	quoteDepth int
}

func (r *mdRenderer) writeByte(c byte) {
	if r.atLine && c != '\n' && r.quoteDepth > 0 && !r.inTable {
		for range r.quoteDepth {
			r.b.WriteString("> ")
		}
	}
	r.b.WriteByte(c)
	r.atLine = c == '\n'
}

func (r *mdRenderer) writeStr(s string) {
	for i := 0; i < len(s); i++ {
		r.writeByte(s[i])
	}
}

func (r *mdRenderer) newline() {
	if !r.atLine {
		r.b.WriteByte('\n')
		r.atLine = true
	}
}

func (r *mdRenderer) blankLine() {
	if r.b.Len() == 0 {
		return
	}
	r.newline()
	s := r.b.String()
	if len(s) >= 2 && s[len(s)-2] == '\n' {
		return
	}
	if r.quoteDepth > 0 {
		for i := range r.quoteDepth {
			if i > 0 {
				r.b.WriteByte(' ')
			}
			r.b.WriteByte('>')
		}
	}
	r.b.WriteByte('\n')
	r.atLine = true
}

func (r *mdRenderer) walkChildren(n *domNode) {
	for _, ch := range n.Children {
		r.walk(ch)
	}
}

func (r *mdRenderer) walk(n *domNode) {
	if n == nil {
		return
	}
	switch n.Kind {
	case domDocument:
		r.walkChildren(n)
	case domText:
		r.renderText(n.Text)
	case domElement:
		r.renderElement(n)
	}
}

func (r *mdRenderer) renderText(s string) {
	if s == "" {
		return
	}
	if r.inPre {
		for _, line := range strings.SplitAfter(s, "\n") {
			if line != "" {
				r.writeStr(line)
			}
		}
		return
	}
	leadSpace := isHTMLSpace(s[0])
	trailSpace := isHTMLSpace(s[len(s)-1])
	words := strings.Fields(s)
	if len(words) == 0 {
		if !r.atLine && r.b.Len() > 0 && !endsWithSpace(r.b.String()) {
			r.writeByte(' ')
		}
		return
	}
	if leadSpace && !r.atLine && !endsWithSpace(r.b.String()) {
		r.writeByte(' ')
	}
	for i, w := range words {
		if i > 0 {
			r.writeByte(' ')
		}
		if r.inCode {
			if r.inTable {
				w = strings.ReplaceAll(w, "|", `\|`)
			}
			r.writeStr(w)
		} else {
			r.writeEscaped(w)
		}
	}
	if trailSpace && !r.atLine && !endsWithSpace(r.b.String()) {
		r.writeByte(' ')
	}
}

func (r *mdRenderer) writeEscaped(word string) {
	for i := 0; i < len(word); i++ {
		c := word[i]
		esc := false
		switch c {
		case '\\', '`', '*', '[', ']':
			esc = true
		case '|':
			esc = r.inTable
		case '#', '-', '+':
			esc = r.atLine && i == 0
		}
		if esc {
			r.writeByte('\\')
		}
		r.writeByte(c)
	}
}

func (r *mdRenderer) renderElement(el *domNode) {
	if isHiddenElement(el) {
		return
	}
	if r.stripShell && isChromeElement(el) {
		return
	}

	switch el.Tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		txt := collapseSpaces(el.innerText())
		if txt == "" {
			return
		}
		lvl := int(el.Tag[1] - '0')
		r.blankLine()
		r.writeStr(strings.Repeat("#", lvl) + " ")
		r.walkChildren(el)
		r.trimTrailingSpace()
		r.blankLine()

	case "p":
		if r.inTable || r.listDepth > 0 {
			if !r.atLine {
				r.newline()
			}
			r.walkChildren(el)
			r.trimTrailingSpace()
			r.newline()
			return
		}
		r.blankLine()
		r.walkChildren(el)
		r.trimTrailingSpace()
		r.blankLine()

	case "br":
		if r.inTable {
			r.writeByte(' ')
		} else {
			r.trimTrailingSpace()
			r.newline()
		}

	case "hr":
		r.blankLine()
		r.writeStr("---")
		r.blankLine()

	case "pre":
		code := strings.TrimRight(el.textContent(), " \t\r\n")
		if strings.TrimSpace(code) == "" {
			return
		}
		lang := detectCodeLang(el)
		fence := strings.Repeat("`", max(3, maxBacktickRun(code)+1))
		r.blankLine()
		r.writeStr(fence + lang + "\n")
		prev := r.inPre
		r.inPre = true
		r.writeStr(code)
		r.inPre = prev
		r.newline()
		r.writeStr(fence)
		r.blankLine()

	case "code", "kbd", "samp":
		if r.inPre {
			r.walkChildren(el)
			return
		}
		raw := collapseSpaces(el.textContent())
		if raw == "" {
			return
		}
		ticks := strings.Repeat("`", maxBacktickRun(raw)+1)
		r.writeStr(ticks)
		if len(ticks) > 1 || strings.HasPrefix(raw, "`") || strings.HasSuffix(raw, "`") {
			r.writeByte(' ')
		}
		if r.inTable {
			raw = strings.ReplaceAll(raw, "|", `\|`)
		}
		r.writeStr(raw)
		if len(ticks) > 1 || strings.HasPrefix(raw, "`") || strings.HasSuffix(raw, "`") {
			r.writeByte(' ')
		}
		r.writeStr(ticks)

	case "b", "strong":
		if strings.TrimSpace(el.innerText()) == "" {
			return
		}
		r.writeStr("**")
		r.walkChildren(el)
		r.trimTrailingSpace()
		r.writeStr("**")

	case "i", "em":
		if strings.TrimSpace(el.innerText()) == "" {
			return
		}
		r.writeStr("*")
		r.walkChildren(el)
		r.trimTrailingSpace()
		r.writeStr("*")

	case "s", "del", "strike":
		if strings.TrimSpace(el.innerText()) == "" {
			return
		}
		r.writeStr("~~")
		r.walkChildren(el)
		r.trimTrailingSpace()
		r.writeStr("~~")

	case "blockquote":
		r.blankLine()
		r.quoteDepth++
		r.walkChildren(el)
		r.trimTrailingSpace()
		r.newline()
		r.quoteDepth--
		r.blankLine()

	case "ul", "menu":
		if r.listDepth == 0 {
			r.blankLine()
		} else {
			r.newline()
		}
		r.renderList(el, false)
		if r.listDepth == 0 {
			r.blankLine()
		}

	case "ol":
		if r.listDepth == 0 {
			r.blankLine()
		} else {
			r.newline()
		}
		r.renderList(el, true)
		if r.listDepth == 0 {
			r.blankLine()
		}

	case "dl":
		r.blankLine()
		for _, ch := range el.Children {
			if ch.Kind != domElement || isHiddenElement(ch) {
				continue
			}
			switch ch.Tag {
			case "dt":
				r.newline()
				r.writeStr("**")
				r.walkChildren(ch)
				r.trimTrailingSpace()
				r.writeStr("**")
				r.newline()
			case "dd":
				r.newline()
				r.writeStr(": ")
				r.walkChildren(ch)
				r.trimTrailingSpace()
				r.newline()
			default:
				r.walk(ch)
			}
		}
		r.blankLine()

	case "a":
		href := strings.TrimSpace(el.attrVal("href"))
		if href == "" || r.inLink || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			r.walkChildren(el)
			return
		}
		resolved := r.doc.resolveURL(href)
		label := collapseSpaces(el.innerText())
		if label == "" {
			label = collapseSpaces(el.attrVal("aria-label"))
		}
		if label == "" {
			label = collapseSpaces(el.attrVal("title"))
		}
		if label == "" {
			return
		}
		if containsBlock(el) {
			r.newline()
			r.writeByte('[')
			r.writeEscaped(label)
			r.writeStr("](")
			r.writeStr(escapeLinkDest(resolved))
			r.writeByte(')')
			r.newline()
			return
		}
		r.inLink = true
		r.writeByte('[')
		if strings.TrimSpace(el.innerText()) != "" {
			r.walkChildren(el)
			r.trimTrailingSpace()
		} else {
			r.writeEscaped(label)
		}
		r.writeStr("](")
		r.writeStr(escapeLinkDest(resolved))
		r.writeByte(')')
		r.inLink = false

	case "img":
		alt := collapseSpaces(el.attrVal("alt"))
		src := strings.TrimSpace(el.attrVal("src"))
		if alt == "" || src == "" || strings.HasPrefix(src, "data:") {
			return
		}
		r.writeStr("![")
		r.writeEscaped(alt)
		r.writeStr("](")
		r.writeStr(escapeLinkDest(r.doc.resolveURL(src)))
		r.writeByte(')')

	case "table":
		r.renderTable(el)

	default:
		if blockTags[el.Tag] && !r.inTable {
			r.newline()
			r.walkChildren(el)
			r.trimTrailingSpace()
			r.newline()
			return
		}
		r.walkChildren(el)
	}
}

func (r *mdRenderer) renderList(list *domNode, ordered bool) {
	num := 1
	if ordered {
		if n, err := strconv.Atoi(strings.TrimSpace(list.attrVal("start"))); err == nil && n >= 0 && n <= 999_999_999 {
			num = n
		}
	}
	indent := strings.Repeat("  ", r.listDepth)
	r.listDepth++
	for _, ch := range list.Children {
		if ch.Kind != domElement || isHiddenElement(ch) {
			continue
		}
		if ch.Tag != "li" {
			r.walk(ch)
			continue
		}
		if strings.TrimSpace(ch.innerText()) == "" {
			continue
		}
		r.newline()
		r.writeStr(indent)
		if ordered {
			r.writeStr(strconv.Itoa(num) + ". ")
			num++
		} else {
			r.writeStr("- ")
		}
		r.walkChildren(ch)
		r.trimTrailingSpace()
		r.newline()
	}
	r.listDepth--
}

func (r *mdRenderer) renderTable(tbl *domNode) {
	var rows [][]*domNode
	var collectRows func(*domNode)
	collectRows = func(n *domNode) {
		for _, ch := range n.Children {
			if ch.Kind != domElement || isHiddenElement(ch) {
				continue
			}
			switch ch.Tag {
			case "table":
				// Nested layout tables stay separate.
			case "tr":
				var cells []*domNode
				for _, c := range ch.Children {
					if c.Kind == domElement && (c.Tag == "td" || c.Tag == "th") && !isHiddenElement(c) {
						cells = append(cells, c)
					}
				}
				if len(cells) > 0 {
					rows = append(rows, cells)
				}
			default:
				collectRows(ch)
			}
		}
	}
	collectRows(tbl)
	if len(rows) == 0 {
		return
	}
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return
	}

	r.blankLine()
	for ri, row := range rows {
		r.newline()
		r.writeByte('|')
		for ci := range cols {
			r.writeByte(' ')
			if ci < len(row) {
				sub := &mdRenderer{doc: r.doc, inTable: true, atLine: true}
				sub.walkChildren(row[ci])
				cell := collapseSpaces(strings.ReplaceAll(sub.b.String(), "\n", " "))
				r.b.WriteString(cell)
				r.atLine = false
			}
			r.writeStr(" |")
		}
		r.newline()
		if ri == 0 {
			r.writeByte('|')
			for range cols {
				r.writeStr("---|")
			}
			r.newline()
		}
	}
	r.blankLine()
}

func (r *mdRenderer) trimTrailingSpace() {
	s := r.b.String()
	trimmed := strings.TrimRight(s, " \t")
	if len(trimmed) < len(s) {
		r.b.Reset()
		r.b.WriteString(trimmed)
		r.atLine = len(trimmed) == 0 || trimmed[len(trimmed)-1] == '\n'
	}
}

func detectCodeLang(pre *domNode) string {
	candidates := []*domNode{pre}
	for _, ch := range pre.Children {
		if ch.Kind == domElement && ch.Tag == "code" {
			candidates = append([]*domNode{ch}, candidates...)
			break
		}
	}
	for _, el := range candidates {
		for _, cls := range el.classes() {
			for _, pfx := range []string{"language-", "lang-", "highlight-source-"} {
				if strings.HasPrefix(cls, pfx) {
					lang := cls[len(pfx):]
					if lang != "" && !strings.ContainsRune(lang, '`') {
						return lang
					}
				}
			}
		}
		if dl := strings.TrimSpace(el.attrVal("data-lang")); dl != "" && !strings.ContainsAny(dl, " `\n") {
			return dl
		}
	}
	return ""
}

func maxBacktickRun(s string) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return best
}

func escapeLinkDest(u string) string {
	r := strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29")
	return r.Replace(u)
}

func endsWithSpace(s string) bool {
	return len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n')
}

func collapseBlankLines(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	nl := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			nl++
			if nl <= 2 {
				b.WriteByte('\n')
			}
			continue
		}
		nl = 0
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---- links, semantic tree, interactive elements & structured data -------

type domLink struct {
	NodeID int
	Text   string
	Href   string
}

func (doc *domDoc) collectLinks(limit int) []domLink {
	if doc == nil {
		return nil
	}
	var out []domLink
	var walk func(*domNode)
	walk = func(n *domNode) {
		if limit > 0 && len(out) >= limit {
			return
		}
		if n.Kind == domElement {
			if isHiddenElement(n) {
				return
			}
			if n.Tag == "a" && n.hasAttr("href") {
				raw := strings.TrimSpace(n.attrVal("href"))
				if raw != "" && !strings.HasPrefix(strings.ToLower(raw), "javascript:") {
					href := doc.resolveURL(raw)
					txt := collapseSpaces(n.innerText())
					if txt == "" {
						txt = collapseSpaces(n.attrVal("aria-label"))
					}
					if txt == "" {
						txt = collapseSpaces(n.attrVal("title"))
					}
					out = append(out, domLink{NodeID: n.ID, Text: txt, Href: href})
				}
				return
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(doc.Root)
	return out
}

func (doc *domDoc) renderSemanticTree(root *domNode, maxBytes int) string {
	if doc == nil || root == nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *domNode, depth int)
	walk = func(n *domNode, depth int) {
		if depth > 14 || (maxBytes > 0 && b.Len() > maxBytes) {
			return
		}
		if n.Kind == domElement {
			if isHiddenElement(n) {
				return
			}
			role := semanticRole(n)
			if role != "" {
				indent := strings.Repeat("  ", depth)
				label := accessibleName(doc, n)
				fmt.Fprintf(&b, "%s[%s #%d]", indent, role, n.ID)
				if label != "" {
					fmt.Fprintf(&b, " %q", oneLine(label, 140))
				}
				if n.Tag == "a" {
					if href := doc.resolveURL(n.attrVal("href")); href != "" {
						fmt.Fprintf(&b, " -> %s", href)
					}
				}
				if isFormControl(n) {
					if v := n.controlValue(); v != "" && !strings.EqualFold(n.attrVal("type"), "password") {
						fmt.Fprintf(&b, " value=%q", oneLine(v, 80))
					}
				}
				b.WriteByte('\n')
				if role == "link" || role == "button" || strings.HasPrefix(role, "heading") {
					return
				}
				for _, ch := range n.Children {
					walk(ch, depth+1)
				}
				return
			}
		}
		for _, ch := range n.Children {
			walk(ch, depth)
		}
	}
	walk(root, 0)
	return capUTF8(strings.TrimSpace(b.String()), maxBytes)
}

func semanticRole(el *domNode) string {
	if r := strings.ToLower(strings.TrimSpace(el.attrVal("role"))); r != "" && r != "none" && r != "presentation" {
		return r
	}
	switch el.Tag {
	case "main":
		return "main"
	case "nav":
		return "navigation"
	case "header":
		return "banner"
	case "footer":
		return "contentinfo"
	case "aside":
		return "complementary"
	case "article":
		return "article"
	case "section":
		if el.hasAttr("aria-label") || el.hasAttr("aria-labelledby") {
			return "region"
		}
	case "form":
		return "form"
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return "heading:" + el.Tag
	case "a":
		if el.hasAttr("href") {
			return "link"
		}
	case "button", "summary":
		return "button"
	case "input":
		t := strings.ToLower(strings.TrimSpace(el.attrVal("type")))
		switch t {
		case "submit", "button", "reset":
			return "button"
		case "checkbox":
			return "checkbox"
		case "radio":
			return "radio"
		case "search":
			return "searchbox"
		default:
			return "textbox"
		}
	case "textarea":
		return "textbox"
	case "select":
		return "combobox"
	case "table":
		return "table"
	case "ul", "ol":
		return "list"
	}
	return ""
}

func isFormControl(el *domNode) bool {
	return el.Tag == "input" || el.Tag == "textarea" || el.Tag == "select"
}

func accessibleName(doc *domDoc, el *domNode) string {
	if s := collapseSpaces(el.attrVal("aria-label")); s != "" {
		return s
	}
	if idRef := strings.TrimSpace(el.attrVal("aria-labelledby")); idRef != "" && doc != nil {
		var parts []string
		for _, id := range strings.Fields(idRef) {
			if lbl, _ := doc.query("#" + id); lbl != nil {
				if t := collapseSpaces(lbl.innerText()); t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	if isFormControl(el) {
		if id := el.id(); id != "" && doc != nil {
			for _, lbl := range doc.queryAll("label[for]") {
				if lbl.attrVal("for") == id {
					if t := collapseSpaces(lbl.innerText()); t != "" {
						return t
					}
				}
			}
		}
		if p := el.closest("label"); p != nil {
			if t := collapseSpaces(p.innerText()); t != "" {
				return t
			}
		}
		if ph := collapseSpaces(el.attrVal("placeholder")); ph != "" {
			return ph
		}
		if el.Tag == "input" {
			t := strings.ToLower(el.attrVal("type"))
			if t == "submit" || t == "button" || t == "reset" {
				if v := collapseSpaces(el.attrVal("value")); v != "" {
					return v
				}
			}
		}
		if nm := collapseSpaces(el.attrVal("name")); nm != "" {
			return nm
		}
	}
	switch el.Tag {
	case "a", "button", "summary", "h1", "h2", "h3", "h4", "h5", "h6":
		if t := collapseSpaces(el.innerText()); t != "" {
			return t
		}
	}
	return collapseSpaces(el.attrVal("title"))
}

func (doc *domDoc) renderInteractive(root *domNode, limit, maxBytes int) string {
	if doc == nil || root == nil {
		return ""
	}
	if limit <= 0 {
		limit = 80
	}
	var b strings.Builder
	count := 0
	var walk func(*domNode)
	walk = func(n *domNode) {
		if count >= limit || (maxBytes > 0 && b.Len() > maxBytes) {
			return
		}
		if n.Kind == domElement {
			if isHiddenElement(n) || n.isDisabled() {
				return
			}
			if role := interactiveRole(n); role != "" {
				count++
				label := accessibleName(doc, n)
				fmt.Fprintf(&b, "%d. [node_id=%d] <%s role=%s>", count, n.ID, n.Tag, role)
				if id := n.id(); id != "" {
					fmt.Fprintf(&b, " #%s", id)
				}
				if nm := n.attrVal("name"); nm != "" {
					fmt.Fprintf(&b, " name=%q", nm)
				}
				if label != "" {
					fmt.Fprintf(&b, " %q", oneLine(label, 120))
				}
				if n.Tag == "a" {
					if href := doc.resolveURL(n.attrVal("href")); href != "" {
						fmt.Fprintf(&b, " -> %s", href)
					}
				}
				if isFormControl(n) {
					if v := n.controlValue(); v != "" && !strings.EqualFold(n.attrVal("type"), "password") {
						fmt.Fprintf(&b, " value=%q", oneLine(v, 60))
					}
				}
				b.WriteByte('\n')
				if n.Tag == "a" || n.Tag == "button" || n.Tag == "select" {
					return
				}
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(root)
	if count == 0 {
		return "(На странице не найдено интерактивных элементов.)"
	}
	return capUTF8(strings.TrimSpace(b.String()), maxBytes)
}

func interactiveRole(el *domNode) string {
	switch el.Tag {
	case "a":
		if el.hasAttr("href") {
			return "link"
		}
	case "button", "summary":
		return "button"
	case "input", "textarea", "select":
		return semanticRole(el)
	}
	switch strings.ToLower(strings.TrimSpace(el.attrVal("role"))) {
	case "button", "link", "tab", "menuitem", "checkbox", "radio", "switch", "textbox", "searchbox", "combobox":
		return strings.ToLower(strings.TrimSpace(el.attrVal("role")))
	}
	return ""
}

// renderStructuredData extracts JSON-LD, OpenGraph, Twitter Card, standard meta
// tags and canonical/alternate links from the page.
func (doc *domDoc) renderStructuredData(maxBytes int) string {
	if doc == nil {
		return ""
	}
	type kv struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	out := map[string]any{}
	if t := doc.title(); t != "" {
		out["title"] = t
	}
	var meta, og, twitter, links []kv
	for _, m := range doc.queryAll("meta[content]") {
		val := strings.TrimSpace(m.attrVal("content"))
		if val == "" {
			continue
		}
		if prop := strings.ToLower(strings.TrimSpace(m.attrVal("property"))); strings.HasPrefix(prop, "og:") {
			og = append(og, kv{Key: prop, Value: val})
			continue
		}
		name := strings.ToLower(strings.TrimSpace(m.attrVal("name")))
		switch {
		case strings.HasPrefix(name, "twitter:"):
			twitter = append(twitter, kv{Key: name, Value: val})
		case name == "description" || name == "keywords" || name == "author" || name == "application-name" || name == "generator":
			meta = append(meta, kv{Key: name, Value: val})
		}
	}
	for _, l := range doc.queryAll("link[rel][href]") {
		rel := strings.ToLower(strings.TrimSpace(l.attrVal("rel")))
		if rel == "canonical" || strings.Contains(rel, "alternate") {
			links = append(links, kv{Key: rel, Value: doc.resolveURL(l.attrVal("href"))})
		}
	}
	var jsonLD []any
	for _, sc := range doc.queryAll("script[type]") {
		if strings.EqualFold(strings.TrimSpace(sc.attrVal("type")), "application/ld+json") {
			raw := strings.TrimSpace(sc.textContent())
			var v any
			if json.Unmarshal([]byte(raw), &v) == nil {
				jsonLD = append(jsonLD, v)
			}
		}
	}
	if len(meta) > 0 {
		out["meta"] = meta
	}
	if len(og) > 0 {
		out["openGraph"] = og
	}
	if len(twitter) > 0 {
		out["twitter"] = twitter
	}
	if len(links) > 0 {
		out["links"] = links
	}
	if len(jsonLD) > 0 {
		out["jsonLd"] = jsonLD
	}
	if len(out) == 0 {
		return "(Структурированных данных на странице не найдено.)"
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ""
	}
	return capUTF8(string(data), maxBytes)
}

// embeddedSPAFallback inspects inline JSON hydration blobs (such as Next.js
// __NEXT_DATA__) when a page shipped an empty client-side root and no headless
// JS browser was available to render it.
func (doc *domDoc) embeddedSPAFallback(maxBytes int) string {
	if doc == nil {
		return ""
	}
	var parts []string
	if m := doc.queryAll(`meta[name="description" i], meta[property="og:description" i]`); len(m) > 0 {
		if desc := collapseSpaces(m[0].attrVal("content")); desc != "" {
			parts = append(parts, desc)
		}
	}
	for _, sc := range doc.queryAll(`script#__NEXT_DATA__, script[type="application/json"]`) {
		raw := strings.TrimSpace(sc.textContent())
		if len(raw) < 20 || len(raw) > 512<<10 {
			continue
		}
		var v any
		if json.Unmarshal([]byte(raw), &v) != nil {
			continue
		}
		var texts []string
		collectProseJSON(v, &texts, 40)
		if len(texts) > 0 {
			parts = append(parts, strings.Join(texts, "\n\n"))
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return capUTF8(strings.Join(parts, "\n\n"), maxBytes)
}

func collectProseJSON(v any, out *[]string, limit int) {
	if len(*out) >= limit {
		return
	}
	switch x := v.(type) {
	case string:
		s := collapseSpaces(x)
		if len(s) >= 40 && strings.ContainsRune(s, ' ') && !strings.HasPrefix(s, "http") && !strings.HasPrefix(s, "/") {
			*out = append(*out, s)
		}
	case []any:
		for _, item := range x {
			collectProseJSON(item, out, limit)
		}
	case map[string]any:
		for k, val := range x {
			lk := strings.ToLower(k)
			if lk == "buildid" || lk == "scriptloader" || lk == "runtimeconfig" {
				continue
			}
			collectProseJSON(val, out, limit)
		}
	}
}
