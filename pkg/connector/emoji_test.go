package connector

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

func TestParseCustomEmoji(t *testing.T) {
	sc, key, ok := parseCustomEmoji("<:kekw:uploads/shared/347a1ed103f88e534efcca1d2465236d2.webp>")
	if !ok || sc != "kekw" || key != "uploads/shared/347a1ed103f88e534efcca1d2465236d2.webp" {
		t.Fatalf("got %q %q %v", sc, key, ok)
	}
	if customEmojiTokenFor(sc, key) != "<:kekw:uploads/shared/347a1ed103f88e534efcca1d2465236d2.webp>" {
		t.Fatal("token round trip")
	}
	for _, s := range []string{"👍", "<:kekw:>", "<:ke kw:x>", ":kekw:", "<:kekw:a b>"} {
		if _, _, ok := parseCustomEmoji(s); ok {
			t.Errorf("%q parsed as custom emoji", s)
		}
	}
}

func TestExpandCustomEmoji(t *testing.T) {
	const tok = "<:kekw:uploads/shared/347a1ed103f88e534efcca1d2465236d2e934f4f9971caee7107891761aae83f>"
	if got := replaceEmojiTokens("lol " + tok + "!"); got != "lol :kekw:!" {
		t.Errorf("replaceEmojiTokens: %q", got)
	}
	cases := map[string]string{
		// Plain content with only an emoji (no content_ast), as seen live.
		tok: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"customEmoji","attrs":{"r2Key":"uploads/shared/347a1ed103f88e534efcca1d2465236d2e934f4f9971caee7107891761aae83f","shortcode":"kekw"}}]}]}`,
		"a " + tok + " b\nc": `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a "},{"type":"customEmoji","attrs":{"r2Key":"uploads/shared/347a1ed103f88e534efcca1d2465236d2e934f4f9971caee7107891761aae83f","shortcode":"kekw"}},{"type":"text","text":" b"}]},{"type":"paragraph","content":[{"type":"text","text":"c"}]}]}`,
		"no emoji <:x: here": `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"no emoji <:x: here"}]}]}`,
	}
	for in, want := range cases {
		ast := textToAST(in)
		expandCustomEmoji(ast)
		w, _ := json.Marshal(riftclient.ParseAST(json.RawMessage(want)))
		if b, _ := json.Marshal(ast); string(b) != string(w) {
			t.Errorf("%q:\n got %s\nwant %s", in, b, want)
		}
	}
	// Inline code and code blocks keep the token as text.
	code := riftclient.ParseAST(json.RawMessage(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + tok + `","marks":[{"type":"code"}]}]},{"type":"codeBlock","content":[{"type":"text","text":"` + tok + `"}]}]}`))
	before, _ := json.Marshal(code)
	expandCustomEmoji(code)
	if after, _ := json.Marshal(code); string(after) != string(before) {
		t.Errorf("code was changed: %s", after)
	}
	// Without a usable emoji, a message with no AST stays plain text.
	if c := testClient().astToMatrix(context.Background(), nil, "plain <:x:"); c.Format != "" || c.Body != "plain <:x:" {
		t.Errorf("plain: %+v", c)
	}
}

func TestWithEmojiTokens(t *testing.T) {
	const tok = "<:kekw:uploads/shared/abc>"
	ast := &riftclient.Node{Type: "doc", Content: []*riftclient.Node{{Type: "paragraph", Content: []*riftclient.Node{{Type: "text", Text: "lol " + tok}}}}}
	if got := withEmojiTokens("lol :kekw:", ast); got != "lol "+tok {
		t.Errorf("got %q", got)
	}
	if got := withEmojiTokens("**hi**", riftclient.MarkdownAST("**hi**")); got != "**hi**" {
		t.Errorf("text without emoji should be kept: %q", got)
	}
}
