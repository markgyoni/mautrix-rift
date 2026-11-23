package riftclient

import (
	"encoding/json"
	"testing"
)

func astJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseInlineMarkdown(t *testing.T) {
	cases := map[string]string{
		"plain":             `[{"type":"text","text":"plain"}]`,
		"a **b** c":         `[{"type":"text","text":"a "},{"type":"text","text":"b","marks":[{"type":"bold"}]},{"type":"text","text":" c"}]`,
		"***x***":           `[{"type":"text","text":"x","marks":[{"type":"bold"},{"type":"italic"}]}]`,
		"*it* `co*de*`":     `[{"type":"text","text":"it","marks":[{"type":"italic"}]},{"type":"text","text":" "},{"type":"text","text":"co*de*","marks":[{"type":"code"}]}]`,
		"snake*case*word":   `[{"type":"text","text":"snake*case*word"}]`,
		"** not bold**":     `[{"type":"text","text":"** not bold**"}]`,
		"~~s~~ __u__ ||p||": `[{"type":"text","text":"s","marks":[{"type":"strike"}]},{"type":"text","text":" "},{"type":"text","text":"u","marks":[{"type":"underline"}]},{"type":"text","text":" "},{"type":"text","text":"p","marks":[{"type":"spoiler"}]}]`,
		// Like the web client regex, a body cannot contain its own delimiter character.
		"**a *b* c**":     `[{"type":"text","text":"**a "},{"type":"text","text":"b","marks":[{"type":"italic"}]},{"type":"text","text":" c**"}]`,
		"**a _b_ c**":     `[{"type":"text","text":"a _b_ c","marks":[{"type":"bold"}]}]`,
		"**a ~~b~~**":     `[{"type":"text","text":"a ","marks":[{"type":"bold"}]},{"type":"text","text":"b","marks":[{"type":"bold"},{"type":"strike"}]}]`,
		`\*lit\*`:         `[{"type":"text","text":"*"},{"type":"text","text":"lit"},{"type":"text","text":"*"}]`,
		"x<sup>2</sup>":   `[{"type":"text","text":"x"},{"type":"text","text":"2","marks":[{"type":"superscript"}]}]`,
		"[c=#f00]red[/c]": `[{"type":"text","text":"red","marks":[{"type":"color","value":"#f00"}]}]`,
	}
	for in, want := range cases {
		if got := astJSON(t, ParseInlineMarkdown(in, nil)); got != want {
			t.Errorf("%q:\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestMarkdownASTMergesEscapes(t *testing.T) {
	got := astJSON(t, MarkdownAST("a \\*b\\*\n\nc"))
	want := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a *b*"}]},{"type":"paragraph"},{"type":"paragraph","content":[{"type":"text","text":"c"}]}]}`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if MarkdownAST("plain\ntext").HasFormatting() {
		t.Error("plain text should have no formatting")
	}
}

func TestDMPlaintextEnvelope(t *testing.T) {
	plain := MarkdownAST("hi")
	if got := EncodeDMPlaintext("hi", plain); got != "hi" {
		t.Errorf("plain DM should be raw text, got %q", got)
	}
	bold := MarkdownAST("**hi**")
	enc := EncodeDMPlaintext("**hi**", bold)
	want := `{"v":1,"t":"**hi**","a":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hi","marks":[{"type":"bold"}]}]}]}}`
	if enc != want {
		t.Fatalf("got %s\nwant %s", enc, want)
	}
	text, ast := DecodeDMPlaintext(enc)
	if text != "**hi**" || astJSON(t, ast) != astJSON(t, bold) {
		t.Errorf("round trip: %q %s", text, astJSON(t, ast))
	}
	if text, ast := DecodeDMPlaintext("{not json"); text != "{not json" || ast != nil {
		t.Errorf("non-envelope text should pass through")
	}
	text, _ = DecodeDMPlaintext(`{"v":1,"t":"","a":null,"f":{"c":{},"t":"fwd","a":null}}`)
	if text != "fwd" {
		t.Errorf("forward text: %q", text)
	}
}
