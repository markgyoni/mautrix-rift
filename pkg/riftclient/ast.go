package riftclient

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
)

// Node is one node of a content_ast document (TipTap/ProseMirror JSON). Node and mark names come
// from the web client bundle's AST → markdown serializer:
//
//	blocks: doc, paragraph, heading{level}, subtext, blockquote, codeBlock|code_block{language},
//	        bulletList, orderedList{start}, taskList, listItem{checked}, table, tableRow,
//	        tableHeader, tableCell, horizontalRule|divider, detailsSummary, image{alt}
//	inline: text, hardBreak, mention{id,label,kind?}, channel_mention{id,label},
//	        timestamp{unix,style}, emoji|customEmoji{shortcode}
//	marks:  bold, italic, underline, strike, code, link{href}, spoiler, color (value="#hex"),
//	        superscript, subscript
//
// Unconfirmed against live traffic: only plain-text documents have been captured so far.
type Node struct {
	Type    string         `json:"type"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Text    string         `json:"text,omitempty"`
	Marks   []Mark         `json:"marks,omitempty"`
	Content []*Node        `json:"content,omitempty"`
}

type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
	Value string         `json:"value,omitempty"` // color mark: "#rrggbb"
}

// ParseAST decodes a content_ast value. It returns nil for null, empty or malformed input.
func ParseAST(raw json.RawMessage) *Node {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n Node
	if json.Unmarshal(raw, &n) != nil || n.Type == "" {
		return nil
	}
	return &n
}

// AttrString returns a string attribute (numbers are formatted without a fraction).
func (n *Node) AttrString(key string) string {
	switch v := n.Attrs[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "true"
		}
		return "false"
	}
	return ""
}

func (m Mark) AttrString(key string) string {
	s, _ := m.Attrs[key].(string)
	return s
}

// HasFormatting reports whether the document carries anything beyond paragraphs of unmarked text.
// It mirrors the web client's check that decides whether a DM embeds the AST in its envelope.
func (n *Node) HasFormatting() bool {
	if n == nil {
		return false
	}
	switch n.Type {
	case "doc", "paragraph":
		for _, c := range n.Content {
			if c.HasFormatting() {
				return true
			}
		}
		return false
	case "text":
		return len(n.Marks) > 0 || strings.Contains(n.Text, "<:")
	}
	return true
}

// PlainText flattens a document to text, one line per paragraph.
func (n *Node) PlainText() string {
	var sb strings.Builder
	var walk func(*Node)
	walk = func(n *Node) {
		switch n.Type {
		case "text":
			sb.WriteString(n.Text)
		case "hardBreak":
			sb.WriteByte('\n')
		case "mention":
			sb.WriteString(prefixed("@", firstNonEmptyStr(n.AttrString("label"), n.AttrString("id"))))
		case "channel_mention":
			sb.WriteString(prefixed("#", firstNonEmptyStr(n.AttrString("label"), n.AttrString("id"))))
		case "emoji", "customEmoji":
			if sc := n.AttrString("shortcode"); sc != "" {
				sb.WriteString(":" + sc + ":")
			}
		}
		for i, c := range n.Content {
			if i > 0 && isBlock(c.Type) {
				sb.WriteByte('\n')
			}
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func isBlock(t string) bool {
	switch t {
	case "text", "hardBreak", "mention", "channel_mention", "timestamp", "emoji", "customEmoji":
		return false
	}
	return true
}

func prefixed(p, s string) string {
	if strings.HasPrefix(s, p) {
		return s
	}
	return p + s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// dmEnvelope is the JSON plaintext of a formatted DM: {"v":1,"t":text,"a":ast}. Plain DMs are just
// the raw text. Forwards add "f":{c,t,a}, dice rolls "r", voice notes "vn", embeds "e".
type dmEnvelope struct {
	V int             `json:"v"`
	T string          `json:"t"`
	A json.RawMessage `json:"a"`
	F *struct {
		T string          `json:"t"`
		A json.RawMessage `json:"a"`
	} `json:"f,omitempty"`
	VN *struct {
		Text string `json:"text"`
	} `json:"vn,omitempty"`
}

// EncodeDMPlaintext builds the plaintext the web client encrypts: raw text, or a v1 JSON envelope
// when the AST has formatting.
func EncodeDMPlaintext(text string, ast *Node) string {
	if !ast.HasFormatting() {
		return text
	}
	a, _ := json.Marshal(ast)
	b, _ := json.Marshal(dmEnvelope{V: 1, T: text, A: a})
	return string(b)
}

// DecodeDMPlaintext unwraps a decrypted DM plaintext into its text and optional AST.
func DecodeDMPlaintext(plain string) (string, *Node) {
	if !strings.HasPrefix(plain, "{") {
		return plain, nil
	}
	var env dmEnvelope
	if json.Unmarshal([]byte(plain), &env) != nil || env.V != 1 {
		return plain, nil
	}
	switch {
	case env.F != nil:
		return env.F.T, ParseAST(env.F.A)
	case env.VN != nil && env.VN.Text != "":
		return env.VN.Text, ParseAST(env.A)
	}
	return env.T, ParseAST(env.A)
}

// MarkdownAST builds a document from plain text the way the web composer does on send: one paragraph
// per line, with Rift's inline markdown (***, **, *, ~~, __, `, ||, <sup>, <sub>, \-escapes) turned
// into marks.
func MarkdownAST(text string) *Node {
	doc := &Node{Type: "doc"}
	for _, line := range strings.Split(text, "\n") {
		p := &Node{Type: "paragraph"}
		if line != "" {
			p.Content = mergeText(ParseInlineMarkdown(line, nil))
		}
		doc.Content = append(doc.Content, p)
	}
	return doc
}

const maxInlineDepth = 6

// ParseInlineMarkdown ports the web client's inline markdown tokenizer (the regex alternation in the
// composer bundle), returning text nodes with marks.
func ParseInlineMarkdown(text string, marks []Mark) []*Node {
	return parseInline([]rune(text), marks, 0)
}

func withMark(marks []Mark, m Mark) []Mark {
	out := make([]Mark, 0, len(marks)+1)
	for _, x := range marks {
		if x.Type != m.Type {
			out = append(out, x)
		}
	}
	return append(out, m)
}

func textNode(s string, marks []Mark) *Node {
	n := &Node{Type: "text", Text: s}
	if len(marks) > 0 {
		n.Marks = append([]Mark(nil), marks...)
	}
	return n
}

func parseInline(r []rune, marks []Mark, depth int) []*Node {
	var out []*Node
	inner := func(s []rune, m []Mark) {
		if depth >= maxInlineDepth {
			out = append(out, textNode(string(s), m))
		} else {
			out = append(out, parseInline(s, m, depth+1)...)
		}
	}
	last := 0
	for i := 0; i < len(r); {
		tok := matchInline(r, i)
		if tok == nil {
			i++
			continue
		}
		if i > last {
			out = append(out, textNode(string(r[last:i]), marks))
		}
		body := r[tok.start:tok.end]
		switch tok.kind {
		case "escape":
			out = append(out, textNode(string(body), marks))
		case "code":
			out = append(out, textNode(string(body), withMark(marks, Mark{Type: "code"})))
		case "bolditalic":
			inner(body, withMark(withMark(marks, Mark{Type: "bold"}), Mark{Type: "italic"}))
		case "color":
			inner(body, withMark(marks, Mark{Type: "color", Value: tok.color}))
		default:
			inner(body, withMark(marks, Mark{Type: tok.kind}))
		}
		i = tok.next
		last = i
	}
	if last < len(r) {
		out = append(out, textNode(string(r[last:]), marks))
	}
	return out
}

type inlineTok struct {
	kind       string
	start, end int // body
	next       int
	color      string
}

func hasPrefixAt(r []rune, i int, p string) bool {
	pr := []rune(p)
	if i+len(pr) > len(r) {
		return false
	}
	for j, c := range pr {
		if r[i+j] != c {
			return false
		}
	}
	return true
}

// scanDelimited finds the closing delimiter for a body that may not contain `forbid` (except
// escaped with a backslash). With strict, the body must not start or end with whitespace.
func scanDelimited(r []rune, from int, closer string, forbid rune, strict bool) (end int, ok bool) {
	if from >= len(r) || (strict && unicode.IsSpace(r[from])) {
		return 0, false
	}
	for j := from; j < len(r); j++ {
		if r[j] == '\\' && j+1 < len(r) {
			j++
			continue
		}
		if hasPrefixAt(r, j, closer) {
			if j == from || (strict && (unicode.IsSpace(r[j-1]) || r[j-1] == '\\')) {
				return 0, false
			}
			return j, true
		}
		if r[j] == forbid {
			return 0, false
		}
	}
	return 0, false
}

func isPunct(c rune) bool {
	return (c >= '!' && c <= '/') || (c >= ':' && c <= '@') || (c >= '[' && c <= '`') || (c >= '{' && c <= '~')
}

func matchInline(r []rune, i int) *inlineTok {
	type delim struct {
		open, kind string
		forbid     rune
		strict     bool
	}
	for _, d := range []delim{
		{"***", "bolditalic", '*', true},
		{"**", "bold", '*', true},
		{"*", "italic", '*', true},
		{"~~", "strike", '~', false},
		{"__", "underline", '_', false},
		{"`", "code", '`', false},
		{"||", "spoiler", '|', false},
	} {
		if !hasPrefixAt(r, i, d.open) {
			continue
		}
		// Single-asterisk italics don't start inside a word.
		if d.kind == "italic" && i > 0 && (unicode.IsLetter(r[i-1]) || unicode.IsNumber(r[i-1])) {
			continue
		}
		from := i + len(d.open)
		if d.kind == "code" {
			for j := from; j < len(r); j++ {
				if r[j] == '`' {
					if j > from {
						return &inlineTok{kind: "code", start: from, end: j, next: j + 1}
					}
					break
				}
			}
			continue
		}
		if end, ok := scanDelimited(r, from, d.open, d.forbid, d.strict); ok {
			return &inlineTok{kind: d.kind, start: from, end: end, next: end + len(d.open)}
		}
	}
	if hasPrefixAt(r, i, "[c=#") {
		j := i + 4
		for j < len(r) && j-i-4 < 6 && strings.ContainsRune("0123456789abcdefABCDEF", r[j]) {
			j++
		}
		if n := j - i - 4; n >= 3 && hasPrefixAt(r, j, "]") {
			for k := j + 1; k < len(r); k++ {
				if hasPrefixAt(r, k, "[/c]") {
					return &inlineTok{kind: "color", color: string(r[i+3 : j]), start: j + 1, end: k, next: k + 4}
				}
			}
		}
	}
	if r[i] == '\\' && i+1 < len(r) && isPunct(r[i+1]) {
		return &inlineTok{kind: "escape", start: i + 1, end: i + 2, next: i + 2}
	}
	for _, t := range []struct{ tag, kind string }{{"sup", "superscript"}, {"sub", "subscript"}} {
		open, closer := "<"+t.tag+">", "</"+t.tag+">"
		if hasPrefixAt(r, i, open) {
			if end, ok := scanDelimited(r, i+len(open), closer, '<', false); ok {
				return &inlineTok{kind: t.kind, start: i + len(open), end: end, next: end + len(closer)}
			}
		}
	}
	return nil
}

// mergeText joins adjacent text nodes with identical marks.
func mergeText(nodes []*Node) []*Node {
	var out []*Node
	for _, n := range nodes {
		if n.Type == "text" && n.Text == "" {
			continue
		}
		if l := len(out); l > 0 && n.Type == "text" && out[l-1].Type == "text" && sameMarks(out[l-1].Marks, n.Marks) {
			out[l-1] = &Node{Type: "text", Text: out[l-1].Text + n.Text, Marks: out[l-1].Marks}
			continue
		}
		out = append(out, n)
	}
	return out
}

func sameMarks(a, b []Mark) bool {
	if len(a) != len(b) {
		return false
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// MergeText is exported for converters that build nodes incrementally.
func MergeText(nodes []*Node) []*Node { return mergeText(nodes) }
