package connector

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

// ---- Rift content_ast → Matrix HTML ----

// astToMatrix builds Matrix message content from a Rift document. body is the Rift-side plain text
// (the message's content field, or the DM envelope text); the AST supplies formatted_body.
func (rc *RiftClient) astToMatrix(ctx context.Context, ast *riftclient.Node, body string) *event.MessageEventContent {
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	if ast == nil {
		return content
	}
	if body == "" {
		content.Body = ast.PlainText()
	}
	if !ast.HasFormatting() {
		return content
	}
	r := &htmlRenderer{rc: rc, ctx: ctx, mentions: &event.Mentions{}}
	var sb strings.Builder
	r.blocks(&sb, ast.Content)
	content.Format = event.FormatHTML
	content.FormattedBody = strings.TrimSuffix(sb.String(), "<br>")
	if len(r.mentions.UserIDs) > 0 {
		content.Mentions = r.mentions
	}
	return content
}

type htmlRenderer struct {
	rc       *RiftClient
	ctx      context.Context
	mentions *event.Mentions
}

func (r *htmlRenderer) blocks(sb *strings.Builder, nodes []*riftclient.Node) {
	for i, n := range nodes {
		// Consecutive paragraphs are lines; Matrix clients render <p> with margins, so use <br>.
		if n.Type == "paragraph" {
			r.inline(sb, n.Content)
			if i < len(nodes)-1 {
				sb.WriteString("<br>")
			}
			continue
		}
		r.block(sb, n)
	}
}

func (r *htmlRenderer) block(sb *strings.Builder, n *riftclient.Node) {
	switch n.Type {
	case "heading":
		lvl, _ := strconv.Atoi(n.AttrString("level"))
		lvl = min(max(lvl, 1), 6)
		fmt.Fprintf(sb, "<h%d>", lvl)
		r.inline(sb, n.Content)
		fmt.Fprintf(sb, "</h%d>", lvl)
	case "subtext":
		sb.WriteString("<p><sub>")
		r.inline(sb, n.Content)
		sb.WriteString("</sub></p>")
	case "blockquote":
		sb.WriteString("<blockquote>")
		r.blocks(sb, n.Content)
		sb.WriteString("</blockquote>")
	case "codeBlock", "code_block":
		var code strings.Builder
		for _, c := range n.Content {
			code.WriteString(c.Text)
		}
		if lang := n.AttrString("language"); lang != "" {
			fmt.Fprintf(sb, `<pre><code class="language-%s">`, html.EscapeString(lang))
		} else {
			sb.WriteString("<pre><code>")
		}
		sb.WriteString(html.EscapeString(code.String()))
		sb.WriteString("</code></pre>")
	case "bulletList", "taskList":
		sb.WriteString("<ul>")
		r.listItems(sb, n.Content, n.Type == "taskList")
		sb.WriteString("</ul>")
	case "orderedList":
		if start := n.AttrString("start"); start != "" && start != "1" {
			fmt.Fprintf(sb, `<ol start="%s">`, html.EscapeString(start))
		} else {
			sb.WriteString("<ol>")
		}
		r.listItems(sb, n.Content, false)
		sb.WriteString("</ol>")
	case "table":
		sb.WriteString("<table>")
		for _, row := range n.Content {
			sb.WriteString("<tr>")
			for _, cell := range row.Content {
				tag := "td"
				if cell.Type == "tableHeader" {
					tag = "th"
				}
				sb.WriteString("<" + tag + ">")
				r.blocks(sb, cell.Content)
				sb.WriteString("</" + tag + ">")
			}
			sb.WriteString("</tr>")
		}
		sb.WriteString("</table>")
	case "horizontalRule", "divider":
		sb.WriteString("<hr>")
	case "image":
		sb.WriteString(html.EscapeString(n.AttrString("alt")))
	default:
		if len(n.Content) > 0 && isInlineNode(n.Content[0]) {
			r.inline(sb, n.Content)
			sb.WriteString("<br>")
		} else {
			r.blocks(sb, n.Content)
		}
	}
}

func isInlineNode(n *riftclient.Node) bool {
	switch n.Type {
	case "text", "hardBreak", "mention", "channel_mention", "timestamp", "emoji", "customEmoji":
		return true
	}
	return false
}

func (r *htmlRenderer) listItems(sb *strings.Builder, items []*riftclient.Node, task bool) {
	for _, item := range items {
		sb.WriteString("<li>")
		if task {
			if item.AttrString("checked") == "true" {
				sb.WriteString("☑ ")
			} else {
				sb.WriteString("☐ ")
			}
		}
		// A list item usually holds a single paragraph; render it inline to avoid stray breaks.
		if len(item.Content) == 1 && item.Content[0].Type == "paragraph" {
			r.inline(sb, item.Content[0].Content)
		} else {
			r.blocks(sb, item.Content)
		}
		sb.WriteString("</li>")
	}
}

// markOrder keeps nesting stable: links outermost, code innermost.
var markOrder = map[string]int{"link": 0, "spoiler": 1, "color": 2, "textStyle": 2, "bold": 3, "italic": 4, "underline": 5, "strike": 6, "superscript": 7, "subscript": 8, "code": 9}

func (r *htmlRenderer) inline(sb *strings.Builder, nodes []*riftclient.Node) {
	for _, n := range nodes {
		var inner string
		switch n.Type {
		case "text":
			inner = strings.ReplaceAll(html.EscapeString(n.Text), "\n", "<br>")
		case "hardBreak":
			sb.WriteString("<br>")
			continue
		case "mention":
			inner = r.mention(n)
		case "channel_mention":
			inner = html.EscapeString(prefixOnce("#", firstNonEmpty(n.AttrString("label"), n.AttrString("id"))))
		case "timestamp":
			inner = html.EscapeString(formatTimestamp(n.AttrString("unix")))
		case "emoji", "customEmoji":
			inner = r.emoji(n)
		default:
			var sub strings.Builder
			r.inline(&sub, n.Content)
			inner = sub.String()
		}
		sb.WriteString(wrapMarks(inner, n.Marks))
	}
}

func wrapMarks(inner string, marks []riftclient.Mark) string {
	sorted := append([]riftclient.Mark(nil), marks...)
	// Insertion sort by markOrder, innermost last; then wrap from the inside out.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && markOrder[sorted[j].Type] < markOrder[sorted[j-1].Type]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	for i := len(sorted) - 1; i >= 0; i-- {
		m := sorted[i]
		switch m.Type {
		case "bold":
			inner = "<strong>" + inner + "</strong>"
		case "italic":
			inner = "<em>" + inner + "</em>"
		case "underline":
			inner = "<u>" + inner + "</u>"
		case "strike", "strikethrough":
			inner = "<del>" + inner + "</del>"
		case "code":
			inner = "<code>" + inner + "</code>"
		case "superscript":
			inner = "<sup>" + inner + "</sup>"
		case "subscript":
			inner = "<sub>" + inner + "</sub>"
		case "spoiler":
			inner = "<span data-mx-spoiler>" + inner + "</span>"
		case "textStyle":
			// The editor stores super/subscript and colour as one mark: {script: super|sub|null, color}.
			switch m.AttrString("script") {
			case "super":
				inner = "<sup>" + inner + "</sup>"
			case "sub":
				inner = "<sub>" + inner + "</sub>"
			}
			if c := m.AttrString("color"); hexColor.MatchString(c) {
				inner = fmt.Sprintf(`<font color="%s" data-mx-color="%s">%s</font>`, c, c, inner)
			}
		case "color":
			if c := m.Value; hexColor.MatchString(c) {
				inner = fmt.Sprintf(`<font color="%s" data-mx-color="%s">%s</font>`, c, c, inner)
			}
		case "link":
			if href := m.AttrString("href"); safeHref(href) {
				inner = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(href), inner)
			}
		}
	}
	return inner
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{3,6}$`)

func safeHref(href string) bool {
	l := strings.ToLower(href)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:")
}

func (r *htmlRenderer) mention(n *riftclient.Node) string {
	userID, label := n.AttrString("id"), prefixOnce("@", firstNonEmpty(n.AttrString("label"), n.AttrString("id")))
	// Role mentions (kind=role/role_all) and unknown users stay plain text.
	if userID == "" || n.AttrString("kind") != "" {
		return html.EscapeString(label)
	}
	var mxid id.UserID
	if userID == string(r.rc.login.ID) {
		mxid = r.rc.login.UserMXID
	} else if ghost, err := r.rc.main.Bridge.GetGhostByID(r.ctx, networkid.UserID(userID)); err == nil && ghost != nil {
		mxid = ghost.Intent.GetMXID()
	}
	if mxid == "" {
		return html.EscapeString(label)
	}
	r.mentions.Add(mxid)
	return fmt.Sprintf(`<a href="%s">%s</a>`, mxid.URI().MatrixToURL(), html.EscapeString(label))
}

func formatTimestamp(unix string) string {
	sec, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return unix
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func prefixOnce(p, s string) string {
	if strings.HasPrefix(s, p) {
		return s
	}
	return p + s
}

// ---- Matrix → Rift content_ast ----

// matrixToRift returns the content string and AST to send for a Matrix message. HTML bodies are
// converted node by node; plain bodies go through Rift's inline markdown, as the web composer does.
func (rc *RiftClient) matrixToRift(ctx context.Context, content *event.MessageEventContent, body string) *riftclient.Node {
	if content.Format != event.FormatHTML || content.FormattedBody == "" {
		return riftclient.MarkdownAST(body)
	}
	nodes, err := nethtml.ParseFragment(strings.NewReader(content.FormattedBody), &nethtml.Node{Type: nethtml.ElementNode, DataAtom: atom.Div, Data: "div"})
	if err != nil {
		return riftclient.MarkdownAST(body)
	}
	c := &htmlParser{rc: rc, ctx: ctx}
	doc := &riftclient.Node{Type: "doc", Content: c.blocks(nodes)}
	if len(doc.Content) == 0 {
		return riftclient.MarkdownAST(body)
	}
	return doc
}

type htmlParser struct {
	rc  *RiftClient
	ctx context.Context
}

func isBlockElement(n *nethtml.Node) bool {
	if n.Type != nethtml.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.P, atom.Div, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Blockquote, atom.Pre,
		atom.Ul, atom.Ol, atom.Li, atom.Hr, atom.Table, atom.Details, atom.Summary:
		return true
	}
	return false
}

func children(n *nethtml.Node) []*nethtml.Node {
	var out []*nethtml.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

// blocks converts sibling HTML nodes to block nodes; runs of inline content become paragraphs,
// split at <br> the way the web client stores one line per paragraph.
func (p *htmlParser) blocks(nodes []*nethtml.Node) []*riftclient.Node {
	var out, pending []*riftclient.Node
	flush := func() {
		blank := true
		for _, n := range pending {
			if n.Type != "text" || strings.TrimSpace(n.Text) != "" {
				blank = false
				break
			}
		}
		if blank {
			pending = nil
			return
		}
		para := &riftclient.Node{Type: "paragraph"}
		for _, n := range append(pending, &riftclient.Node{Type: "hardBreak"}) {
			if n.Type == "hardBreak" {
				para.Content = riftclient.MergeText(trimInline(para.Content))
				out = append(out, para)
				para = &riftclient.Node{Type: "paragraph"}
				continue
			}
			para.Content = append(para.Content, n)
		}
		pending = nil
	}
	for _, n := range nodes {
		if n.Type == nethtml.ElementNode && n.Data == "mx-reply" {
			continue
		}
		if !isBlockElement(n) {
			pending = append(pending, p.inline(n, nil)...)
			continue
		}
		flush()
		out = append(out, p.block(n)...)
	}
	flush()
	return out
}

func trimInline(nodes []*riftclient.Node) []*riftclient.Node {
	if len(nodes) > 0 && nodes[0].Type == "text" {
		nodes[0] = &riftclient.Node{Type: "text", Text: strings.TrimLeft(nodes[0].Text, " \t\n"), Marks: nodes[0].Marks}
	}
	if l := len(nodes); l > 0 && nodes[l-1].Type == "text" {
		nodes[l-1] = &riftclient.Node{Type: "text", Text: strings.TrimRight(nodes[l-1].Text, " \t\n"), Marks: nodes[l-1].Marks}
	}
	return nodes
}

func (p *htmlParser) block(n *nethtml.Node) []*riftclient.Node {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := float64(n.Data[1] - '0')
		return []*riftclient.Node{{Type: "heading", Attrs: map[string]any{"level": level}, Content: riftclient.MergeText(trimInline(p.inlineChildren(n, nil)))}}
	case atom.Blockquote:
		return []*riftclient.Node{{Type: "blockquote", Content: p.blocks(children(n))}}
	case atom.Pre:
		code, lang := textContent(n), ""
		if c := n.FirstChild; c != nil && c.DataAtom == atom.Code {
			for _, a := range c.Attr {
				if a.Key == "class" {
					for _, cls := range strings.Fields(a.Val) {
						if strings.HasPrefix(cls, "language-") {
							lang = strings.TrimPrefix(cls, "language-")
						}
					}
				}
			}
		}
		node := &riftclient.Node{Type: "codeBlock", Attrs: map[string]any{"language": nil}}
		if lang != "" {
			node.Attrs["language"] = lang
		}
		if code = strings.TrimSuffix(code, "\n"); code != "" {
			node.Content = []*riftclient.Node{{Type: "text", Text: code}}
		}
		return []*riftclient.Node{node}
	case atom.Ul, atom.Ol:
		list := &riftclient.Node{Type: "bulletList"}
		if n.DataAtom == atom.Ol {
			list.Type = "orderedList"
			start := 1.0
			if s, err := strconv.Atoi(attr(n, "start")); err == nil {
				start = float64(s)
			}
			list.Attrs = map[string]any{"start": start}
		}
		for _, c := range children(n) {
			if c.Type == nethtml.ElementNode && c.DataAtom == atom.Li {
				item := &riftclient.Node{Type: "listItem", Content: p.blocks(children(c))}
				if len(item.Content) == 0 {
					item.Content = []*riftclient.Node{{Type: "paragraph"}}
				}
				list.Content = append(list.Content, item)
			}
		}
		return []*riftclient.Node{list}
	case atom.Hr:
		return []*riftclient.Node{{Type: "horizontalRule"}}
	case atom.Table:
		table := &riftclient.Node{Type: "table"}
		var rows func(*nethtml.Node)
		rows = func(n *nethtml.Node) {
			for _, c := range children(n) {
				switch c.DataAtom {
				case atom.Thead, atom.Tbody, atom.Tfoot:
					rows(c)
				case atom.Tr:
					row := &riftclient.Node{Type: "tableRow"}
					for _, cell := range children(c) {
						if cell.DataAtom == atom.Td || cell.DataAtom == atom.Th {
							typ := "tableCell"
							if cell.DataAtom == atom.Th {
								typ = "tableHeader"
							}
							cellBlocks := p.blocks(children(cell))
							if len(cellBlocks) == 0 {
								cellBlocks = []*riftclient.Node{{Type: "paragraph"}}
							}
							row.Content = append(row.Content, &riftclient.Node{Type: typ, Content: cellBlocks})
						}
					}
					table.Content = append(table.Content, row)
				}
			}
		}
		rows(n)
		return []*riftclient.Node{table}
	}
	// p, div, li, details, summary: just their contents.
	return p.blocks(children(n))
}

func attr(n *nethtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *nethtml.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func textContent(n *nethtml.Node) string {
	if n.Type == nethtml.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.DataAtom == atom.Br {
			sb.WriteByte('\n')
		}
		sb.WriteString(textContent(c))
	}
	return sb.String()
}

func addMark(marks []riftclient.Mark, m riftclient.Mark) []riftclient.Mark {
	out := make([]riftclient.Mark, 0, len(marks)+1)
	for _, x := range marks {
		if x.Type != m.Type {
			out = append(out, x)
		}
	}
	return append(out, m)
}

// addTextStyle sets one attribute of the editor's textStyle mark, which the web composer emits as
// {"type":"textStyle","attrs":{"color":null|"#hex","script":null|"super"|"sub"}}.
func addTextStyle(marks []riftclient.Mark, key, val string) []riftclient.Mark {
	attrs := map[string]any{"color": nil, "script": nil}
	for _, m := range marks {
		if m.Type == "textStyle" {
			for k, v := range m.Attrs {
				attrs[k] = v
			}
		}
	}
	attrs[key] = val
	return addMark(marks, riftclient.Mark{Type: "textStyle", Attrs: attrs})
}

var whitespaceRun = regexp.MustCompile(`[ \t\r\n]+`)

func (p *htmlParser) inlineChildren(n *nethtml.Node, marks []riftclient.Mark) []*riftclient.Node {
	var out []*riftclient.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, p.inline(c, marks)...)
	}
	return out
}

func (p *htmlParser) inline(n *nethtml.Node, marks []riftclient.Mark) []*riftclient.Node {
	switch n.Type {
	case nethtml.TextNode:
		text := whitespaceRun.ReplaceAllString(n.Data, " ")
		if text == "" {
			return nil
		}
		node := &riftclient.Node{Type: "text", Text: text}
		if len(marks) > 0 {
			node.Marks = append([]riftclient.Mark(nil), marks...)
		}
		return []*riftclient.Node{node}
	case nethtml.ElementNode:
	default:
		return nil
	}
	switch n.DataAtom {
	case atom.Br:
		return []*riftclient.Node{{Type: "hardBreak"}}
	case atom.Strong, atom.B:
		marks = addMark(marks, riftclient.Mark{Type: "bold"})
	case atom.Em, atom.I:
		marks = addMark(marks, riftclient.Mark{Type: "italic"})
	case atom.U, atom.Ins:
		marks = addMark(marks, riftclient.Mark{Type: "underline"})
	case atom.Del, atom.S, atom.Strike:
		marks = addMark(marks, riftclient.Mark{Type: "strike"})
	case atom.Sup:
		marks = addTextStyle(marks, "script", "super")
	case atom.Sub:
		marks = addTextStyle(marks, "script", "sub")
	case atom.Code:
		text := textContent(n)
		if text == "" {
			return nil
		}
		return []*riftclient.Node{{Type: "text", Text: text, Marks: addMark(marks, riftclient.Mark{Type: "code"})}}
	case atom.Img:
		// Emoticons that came from Rift go back as custom emoji; other images degrade to their alt text.
		if node := p.customEmoji(n); node != nil {
			return []*riftclient.Node{node}
		}
		if alt := attr(n, "alt"); alt != "" {
			return []*riftclient.Node{{Type: "text", Text: alt, Marks: marks}}
		}
		return nil
	case atom.A:
		href := attr(n, "href")
		if mention := p.mention(href, textContent(n)); mention != nil {
			if len(marks) > 0 {
				mention.Marks = append([]riftclient.Mark(nil), marks...)
			}
			return []*riftclient.Node{mention}
		}
		if safeHref(href) {
			marks = addMark(marks, riftclient.Mark{Type: "link", Attrs: map[string]any{"href": href}})
		}
	case atom.Span, atom.Font:
		if hasAttr(n, "data-mx-spoiler") {
			marks = addMark(marks, riftclient.Mark{Type: "spoiler"})
		}
		if c := firstNonEmpty(attr(n, "data-mx-color"), attr(n, "color")); hexColor.MatchString(c) {
			marks = addTextStyle(marks, "color", c)
		}
	}
	return p.inlineChildren(n, marks)
}

// mention turns a matrix.to link to a Rift ghost (or the logged-in user) into a mention node.
func (p *htmlParser) mention(href, text string) *riftclient.Node {
	uri, err := id.ParseMatrixURIOrMatrixToURL(href)
	if err != nil || uri.Sigil1 != '@' {
		return nil
	}
	mxid := uri.UserID()
	var userID networkid.UserID
	if mxid == p.rc.login.UserMXID {
		userID = networkid.UserID(p.rc.login.ID)
	} else if ghostID, ok := p.rc.main.Bridge.Matrix.ParseGhostMXID(mxid); ok {
		userID = ghostID
	} else {
		return nil
	}
	label := strings.TrimPrefix(strings.TrimSpace(text), "@")
	if label == "" {
		label = string(userID)
	}
	return &riftclient.Node{Type: "mention", Attrs: map[string]any{"id": string(userID), "label": label}}
}

// emoji renders an emoji node; custom emoji with an image become inline Matrix emoticons.
func (r *htmlRenderer) emoji(n *riftclient.Node) string {
	sc := n.AttrString("shortcode")
	if sc == "" {
		return ""
	}
	alt := html.EscapeString(":" + sc + ":")
	r2Key := n.AttrString("r2Key")
	if n.Type != "customEmoji" || r2Key == "" {
		return alt
	}
	mxc, err := r.rc.customEmojiMXC(r.ctx, sc, r2Key)
	if err != nil {
		zerolog.Ctx(r.ctx).Warn().Err(err).Str("shortcode", sc).Msg("Failed to bridge custom emoji")
		return alt
	}
	return fmt.Sprintf(`<img data-mx-emoticon height="32" src="%s" alt="%s" title="%s">`, html.EscapeString(string(mxc)), alt, alt)
}

// customEmoji maps an <img> of a bridged Rift emoji back to a customEmoji node.
func (p *htmlParser) customEmoji(n *nethtml.Node) *riftclient.Node {
	src := attr(n, "src")
	if !strings.HasPrefix(src, "mxc://") {
		return nil
	}
	token := p.rc.main.Bridge.DB.KV.Get(p.ctx, kvEmojiToken(id.ContentURIString(src)))
	sc, r2Key, ok := parseCustomEmoji(token)
	if !ok {
		return nil
	}
	return &riftclient.Node{Type: "customEmoji", Attrs: map[string]any{"shortcode": sc, "r2Key": r2Key}}
}
