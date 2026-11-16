package lightpanda

import (
	"fmt"
	"strconv"
	"strings"
)

type cssAttrMatch struct {
	key string
	op  string // "", "=", "~=", "|=", "^=", "$=", "*="
	val string
	ci  bool // case-insensitive flag
}

type cssPseudo struct {
	name string
	arg  string
	not  []cssChain
}

type cssCompound struct {
	tag     string // "" or "*" means any tag
	id      string
	classes []string
	attrs   []cssAttrMatch
	pseudos []cssPseudo
}

type cssStep struct {
	comb byte // 0 = subject, ' ' = descendant, '>' = child, '+' = adjacent, '~' = general sibling
	comp cssCompound
}

// cssChain holds compound selectors from left (ancestor) to right (subject).
type cssChain []cssStep

func (doc *domDoc) query(sel string) (*domNode, error) {
	chains, err := parseSelectorList(sel)
	if err != nil {
		return nil, err
	}
	return queryFirst(doc.Root, chains), nil
}

func (doc *domDoc) queryAllErr(sel string) ([]*domNode, error) {
	chains, err := parseSelectorList(sel)
	if err != nil {
		return nil, err
	}
	var out []*domNode
	queryCollect(doc.Root, chains, &out)
	return out, nil
}

func (doc *domDoc) queryAll(sel string) []*domNode {
	if doc == nil || doc.Root == nil {
		return nil
	}
	out, _ := doc.queryAllErr(sel)
	return out
}

func queryFirst(root *domNode, chains []cssChain) *domNode {
	var found *domNode
	var walk func(*domNode) bool
	walk = func(n *domNode) bool {
		if n.Kind == domElement && n != root && matchesAnyChain(n, chains) {
			found = n
			return true
		}
		for _, ch := range n.Children {
			if walk(ch) {
				return true
			}
		}
		return false
	}
	walk(root)
	return found
}

func queryCollect(root *domNode, chains []cssChain, out *[]*domNode) {
	var walk func(*domNode)
	walk = func(n *domNode) {
		if n.Kind == domElement && n != root && matchesAnyChain(n, chains) {
			*out = append(*out, n)
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(root)
}

func matchesAnyChain(el *domNode, chains []cssChain) bool {
	for _, ch := range chains {
		if len(ch) > 0 && matchChainAt(el, ch, len(ch)-1) {
			return true
		}
	}
	return false
}

func matchChainAt(el *domNode, ch cssChain, idx int) bool {
	if el == nil || el.Kind != domElement {
		return false
	}
	if !matchCompound(el, ch[idx].comp) {
		return false
	}
	if idx == 0 {
		return true
	}
	switch ch[idx].comb {
	case '>':
		return matchChainAt(el.Parent, ch, idx-1)
	case ' ':
		for p := el.Parent; p != nil; p = p.Parent {
			if p.Kind == domElement && matchChainAt(p, ch, idx-1) {
				return true
			}
		}
		return false
	case '+':
		return matchChainAt(prevElementSibling(el), ch, idx-1)
	case '~':
		for s := prevElementSibling(el); s != nil; s = prevElementSibling(s) {
			if matchChainAt(s, ch, idx-1) {
				return true
			}
		}
		return false
	}
	return false
}

func matchCompound(el *domNode, c cssCompound) bool {
	if c.tag != "" && c.tag != "*" && el.Tag != c.tag {
		return false
	}
	if c.id != "" && el.id() != c.id {
		return false
	}
	for _, cls := range c.classes {
		if !el.hasClass(cls) {
			return false
		}
	}
	for _, a := range c.attrs {
		if !matchAttr(el, a) {
			return false
		}
	}
	for _, p := range c.pseudos {
		if !matchPseudo(el, p) {
			return false
		}
	}
	return true
}

func matchAttr(el *domNode, m cssAttrMatch) bool {
	got, ok := el.attr(m.key)
	if !ok {
		return false
	}
	if m.op == "" {
		return true
	}
	want := m.val
	if m.ci {
		got = strings.ToLower(got)
		want = strings.ToLower(want)
	}
	switch m.op {
	case "=":
		return got == want
	case "~=":
		if want == "" {
			return false
		}
		for _, w := range strings.Fields(got) {
			if w == want {
				return true
			}
		}
		return false
	case "|=":
		return got == want || strings.HasPrefix(got, want+"-")
	case "^=":
		return want != "" && strings.HasPrefix(got, want)
	case "$=":
		return want != "" && strings.HasSuffix(got, want)
	case "*=":
		return want != "" && strings.Contains(got, want)
	}
	return false
}

func matchPseudo(el *domNode, p cssPseudo) bool {
	switch p.name {
	case "first-child":
		return prevElementSibling(el) == nil
	case "last-child":
		return nextElementSibling(el) == nil
	case "only-child":
		return prevElementSibling(el) == nil && nextElementSibling(el) == nil
	case "nth-child":
		return matchNth(elementIndex(el), p.arg)
	case "empty":
		for _, ch := range el.Children {
			if ch.Kind == domElement || (ch.Kind == domText && ch.Text != "") {
				return false
			}
		}
		return true
	case "checked":
		return el.isChecked()
	case "disabled":
		return el.isDisabled()
	case "enabled":
		return !el.isDisabled()
	case "not":
		return !matchesAnyChain(el, p.not)
	case "is", "where":
		return matchesAnyChain(el, p.not)
	case "has":
		return queryFirst(el, p.not) != nil
	}
	return false
}

func matchNth(idx int, expr string) bool {
	s := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(expr), " ", ""))
	switch s {
	case "odd":
		return idx%2 == 1
	case "even":
		return idx%2 == 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return idx == n
	}
	nPos := strings.IndexByte(s, 'n')
	if nPos < 0 {
		return false
	}
	aStr, bStr := s[:nPos], s[nPos+1:]
	var a, b int
	var err error
	switch aStr {
	case "", "+":
		a = 1
	case "-":
		a = -1
	default:
		a, err = strconv.Atoi(aStr)
		if err != nil {
			return false
		}
	}
	if bStr != "" {
		b, err = strconv.Atoi(bStr)
		if err != nil {
			return false
		}
	}
	if a == 0 {
		return idx == b
	}
	diff := idx - b
	return diff%a == 0 && diff/a >= 0
}

func elementIndex(el *domNode) int {
	if el.Parent == nil {
		return 1
	}
	idx := 0
	for _, ch := range el.Parent.Children {
		if ch.Kind == domElement {
			idx++
			if ch == el {
				return idx
			}
		}
	}
	return 1
}

func prevElementSibling(el *domNode) *domNode {
	if el == nil || el.Parent == nil {
		return nil
	}
	var prev *domNode
	for _, ch := range el.Parent.Children {
		if ch == el {
			return prev
		}
		if ch.Kind == domElement {
			prev = ch
		}
	}
	return nil
}

func nextElementSibling(el *domNode) *domNode {
	if el == nil || el.Parent == nil {
		return nil
	}
	seen := false
	for _, ch := range el.Parent.Children {
		if ch == el {
			seen = true
			continue
		}
		if seen && ch.Kind == domElement {
			return ch
		}
	}
	return nil
}

// ---- CSS selector parser ------------------------------------------------

type selParser struct {
	s string
	i int
}

func parseSelectorList(raw string) ([]cssChain, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("пустой CSS-селектор")
	}
	p := &selParser{s: raw}
	return p.parseList()
}

func (p *selParser) parseList() ([]cssChain, error) {
	var list []cssChain
	for {
		p.skipSpaces()
		ch, err := p.parseChain()
		if err != nil {
			return nil, err
		}
		list = append(list, ch)
		p.skipSpaces()
		if p.i >= len(p.s) {
			return list, nil
		}
		if p.s[p.i] != ',' {
			return nil, fmt.Errorf("неверный CSS-селектор %q около %q", p.s, p.s[p.i:])
		}
		p.i++
	}
}

func (p *selParser) parseChain() (cssChain, error) {
	var chain cssChain
	var comb byte
	for {
		comp, ok, err := p.parseCompound()
		if err != nil {
			return nil, err
		}
		if !ok {
			if len(chain) == 0 {
				return nil, fmt.Errorf("неверный CSS-селектор %q", p.s)
			}
			return chain, nil
		}
		chain = append(chain, cssStep{comb: comb, comp: comp})

		hadSpace := p.skipSpaces()
		if p.i >= len(p.s) || p.s[p.i] == ',' || p.s[p.i] == ')' {
			return chain, nil
		}
		switch p.s[p.i] {
		case '>', '+', '~':
			comb = p.s[p.i]
			p.i++
			p.skipSpaces()
		default:
			if !hadSpace {
				return nil, fmt.Errorf("неверный CSS-селектор %q около %q", p.s, p.s[p.i:])
			}
			comb = ' '
		}
	}
}

func (p *selParser) parseCompound() (cssCompound, bool, error) {
	var c cssCompound
	any := false

	if p.i < len(p.s) && p.s[p.i] == '*' {
		c.tag = "*"
		p.i++
		any = true
	} else if id := p.readIdent(); id != "" {
		c.tag = strings.ToLower(id)
		any = true
	}

	for p.i < len(p.s) {
		switch p.s[p.i] {
		case '#':
			p.i++
			id := p.readIdent()
			if id == "" {
				return c, false, fmt.Errorf("ожидался идентификатор после # в %q", p.s)
			}
			c.id = id
			any = true
		case '.':
			p.i++
			cls := p.readIdent()
			if cls == "" {
				return c, false, fmt.Errorf("ожидалось имя класса после . в %q", p.s)
			}
			c.classes = append(c.classes, cls)
			any = true
		case '[':
			am, err := p.parseAttr()
			if err != nil {
				return c, false, err
			}
			c.attrs = append(c.attrs, am)
			any = true
		case ':':
			p.i++
			if p.i < len(p.s) && p.s[p.i] == ':' {
				// Pseudo-elements don't exist as DOM nodes; reject cleanly.
				return c, false, fmt.Errorf("псевдоэлементы не поддерживаются в %q", p.s)
			}
			ps, err := p.parsePseudo()
			if err != nil {
				return c, false, err
			}
			c.pseudos = append(c.pseudos, ps)
			any = true
		default:
			return c, any, nil
		}
	}
	return c, any, nil
}

func (p *selParser) parseAttr() (cssAttrMatch, error) {
	p.i++ // '['
	p.skipSpaces()
	key := strings.ToLower(p.readIdent())
	if key == "" {
		return cssAttrMatch{}, fmt.Errorf("ожидалось имя атрибута в %q", p.s)
	}
	p.skipSpaces()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return cssAttrMatch{key: key}, nil
	}
	var op string
	if p.i+1 < len(p.s) && p.s[p.i+1] == '=' {
		switch p.s[p.i] {
		case '~', '|', '^', '$', '*':
			op = p.s[p.i : p.i+2]
			p.i += 2
		}
	}
	if op == "" && p.i < len(p.s) && p.s[p.i] == '=' {
		op = "="
		p.i++
	}
	if op == "" {
		return cssAttrMatch{}, fmt.Errorf("неверный оператор атрибута в %q", p.s)
	}
	p.skipSpaces()
	var val string
	if p.i < len(p.s) && (p.s[p.i] == '"' || p.s[p.i] == '\'') {
		q := p.s[p.i]
		p.i++
		var b strings.Builder
		for p.i < len(p.s) && p.s[p.i] != q {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				p.i++
			}
			b.WriteByte(p.s[p.i])
			p.i++
		}
		if p.i >= len(p.s) {
			return cssAttrMatch{}, fmt.Errorf("незакрытая кавычка в %q", p.s)
		}
		p.i++
		val = b.String()
	} else {
		val = p.readIdent()
	}
	p.skipSpaces()
	ci := false
	if p.i < len(p.s) && (p.s[p.i] == 'i' || p.s[p.i] == 'I') {
		ci = true
		p.i++
		p.skipSpaces()
	} else if p.i < len(p.s) && (p.s[p.i] == 's' || p.s[p.i] == 'S') {
		p.i++
		p.skipSpaces()
	}
	if p.i >= len(p.s) || p.s[p.i] != ']' {
		return cssAttrMatch{}, fmt.Errorf("ожидалась ] в %q", p.s)
	}
	p.i++
	return cssAttrMatch{key: key, op: op, val: val, ci: ci}, nil
}

func (p *selParser) parsePseudo() (cssPseudo, error) {
	name := strings.ToLower(p.readIdent())
	if name == "" {
		return cssPseudo{}, fmt.Errorf("ожидалось имя псевдокласса в %q", p.s)
	}
	ps := cssPseudo{name: name}
	if p.i >= len(p.s) || p.s[p.i] != '(' {
		return ps, nil
	}
	p.i++ // '('
	depth := 1
	start := p.i
	for p.i < len(p.s) && depth > 0 {
		switch p.s[p.i] {
		case '(':
			depth++
		case ')':
			depth--
		case '"', '\'':
			q := p.s[p.i]
			p.i++
			for p.i < len(p.s) && p.s[p.i] != q {
				if p.s[p.i] == '\\' {
					p.i++
				}
				p.i++
			}
		}
		if depth > 0 {
			p.i++
		}
	}
	if depth != 0 {
		return ps, fmt.Errorf("незакрытая ( в %q", p.s)
	}
	ps.arg = strings.TrimSpace(p.s[start:p.i])
	p.i++ // ')'
	if name == "not" || name == "is" || name == "where" || name == "has" {
		sub, err := parseSelectorList(ps.arg)
		if err != nil {
			return ps, err
		}
		ps.not = sub
	}
	return ps, nil
}

func (p *selParser) skipSpaces() bool {
	start := p.i
	for p.i < len(p.s) && isHTMLSpace(p.s[p.i]) {
		p.i++
	}
	return p.i > start
}

func (p *selParser) readIdent() string {
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '\\' && p.i+1 < len(p.s) {
			p.i++
			b.WriteByte(p.s[p.i])
			p.i++
			continue
		}
		if isIdentByte(c) {
			b.WriteByte(c)
			p.i++
			continue
		}
		break
	}
	return b.String()
}

func isIdentByte(b byte) bool {
	return isASCIIAlpha(b) || (b >= '0' && b <= '9') || b == '-' || b == '_' || b >= 0x80
}
