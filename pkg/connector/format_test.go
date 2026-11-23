package connector

import (
	"context"
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

func testClient() *RiftClient {
	return &RiftClient{login: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "me", UserMXID: "@me:beeper.com"}}}
}

func TestHTMLToAST(t *testing.T) {
	rc := testClient()
	cases := map[string]string{
		"<strong>bold</strong> and <em>it</em>": `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"bold","marks":[{"type":"bold"}]},{"type":"text","text":" and "},{"type":"text","text":"it","marks":[{"type":"italic"}]}]}]}`,
		"line1<br>line2":                        `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"line1"}]},{"type":"paragraph","content":[{"type":"text","text":"line2"}]}]}`,
		`<pre><code class="language-go">x := 1
</code></pre>`: `{"type":"doc","content":[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"x := 1"}]}]}`,
		"<ul><li>a</li><li><del>b</del></li></ul>":                   `{"type":"doc","content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"a"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"b","marks":[{"type":"strike"}]}]}]}]}]}`,
		`<a href="https://x.y">l</a> <span data-mx-spoiler>s</span>`: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"l","marks":[{"type":"link","attrs":{"href":"https://x.y"}}]},{"type":"text","text":" "},{"type":"text","text":"s","marks":[{"type":"spoiler"}]}]}]}`,
		`x<sup>2</sup>`:                                          `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"x"},{"type":"text","text":"2","marks":[{"type":"textStyle","attrs":{"color":null,"script":"super"}}]}]}]}`,
		`<blockquote>q</blockquote>after`:                        `{"type":"doc","content":[{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"q"}]}]},{"type":"paragraph","content":[{"type":"text","text":"after"}]}]}`,
		`hi <a href="https://matrix.to/#/@me:beeper.com">Me</a>`: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"hi "},{"type":"mention","attrs":{"id":"me","label":"Me"}}]}]}`,
	}
	for in, want := range cases {
		content := &event.MessageEventContent{Body: "x", Format: event.FormatHTML, FormattedBody: in}
		b, _ := json.Marshal(rc.matrixToRift(context.Background(), content, "x"))
		if string(b) != want {
			t.Errorf("%q:\n got %s\nwant %s", in, b, want)
		}
	}
}

func TestASTToHTML(t *testing.T) {
	rc := testClient()
	cases := map[string]string{
		`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a<b","marks":[{"type":"bold"},{"type":"link","attrs":{"href":"https://x.y"}}]}]},{"type":"paragraph","content":[{"type":"text","text":"c"}]}]}`: `<a href="https://x.y"><strong>a&lt;b</strong></a><br>c`,
		`{"type":"doc","content":[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"x<1"}]}]}`:                                                                                                            `<pre><code class="language-go">x&lt;1</code></pre>`,
		`{"type":"doc","content":[{"type":"orderedList","attrs":{"start":3},"content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"i"}]}]}]}]}`:                                                 `<ol start="3"><li>i</li></ol>`,
		`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"mention","attrs":{"id":"me","label":"Me"}}]}]}`:                                                                                                               `<a href="https://matrix.to/#/@me:beeper.com">@Me</a>`,
		`{"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"H"}]},{"type":"paragraph","content":[{"type":"text","text":"s","marks":[{"type":"spoiler"}]}]}]}`:                             `<h2>H</h2><span data-mx-spoiler>s</span>`,
	}
	// Captured from the web client: superscript is a textStyle mark, and links carry editor attrs.
	cases[`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a"},{"type":"text","text":"asd","marks":[{"attrs":{"color":null,"script":"super"},"type":"textStyle"}]}]},{"type":"paragraph","content":[{"type":"text","text":"ni","marks":[{"attrs":{"class":"text-brand","href":"https://asdasd","rel":"noopener noreferrer","target":"_blank","title":null},"type":"link"},{"attrs":{"color":null,"script":null},"type":"textStyle"}]}]}]}`] = `a<sup>asd</sup><br><a href="https://asdasd">ni</a>`
	for in, want := range cases {
		content := rc.astToMatrix(context.Background(), riftclient.ParseAST(json.RawMessage(in)), "body")
		if content.FormattedBody != want {
			t.Errorf("%s:\n got %s\nwant %s", in, content.FormattedBody, want)
		}
	}
	plain := rc.astToMatrix(context.Background(), riftclient.MarkdownAST("hello"), "hello")
	if plain.Format != "" || plain.Body != "hello" {
		t.Errorf("plain AST should not produce HTML: %+v", plain)
	}
}
