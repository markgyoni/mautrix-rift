package connector

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

// Custom emoji are "<:shortcode:r2Key>" tokens in reactions, and customEmoji{shortcode, r2Key} nodes
// in content_ast (both from the web bundle's emoji picker). The image is a normal R2 key.
var customEmojiToken = regexp.MustCompile(`^<:([A-Za-z0-9_]{1,32}):([^>\s]+)>$`)

func parseCustomEmoji(s string) (shortcode, r2Key string, ok bool) {
	m := customEmojiToken.FindStringSubmatch(s)
	if m == nil || len(m[2]) > 1024 {
		return "", "", false
	}
	return m[1], m[2], true
}

func customEmojiTokenFor(shortcode, r2Key string) string {
	return "<:" + shortcode + ":" + r2Key + ">"
}

// In messages, the web client sends custom emoji as the same tokens inside plain text (the content
// field and content_ast text nodes, seen live), and renders them by scanning text for
// /<:[a-z0-9_]+:[^>]+>/. So tokens have to be found in text, not only in customEmoji nodes.
var customEmojiInText = regexp.MustCompile(`<:([A-Za-z0-9_]{1,32}):([^>\s]{1,1000})>`)

// replaceEmojiTokens turns custom emoji tokens into ":shortcode:" for plain-text bodies.
func replaceEmojiTokens(s string) string {
	return customEmojiInText.ReplaceAllString(s, ":$1:")
}

// textToAST wraps plain text in a document (one unformatted paragraph per line).
func textToAST(text string) *riftclient.Node {
	doc := &riftclient.Node{Type: "doc"}
	for _, line := range strings.Split(text, "\n") {
		p := &riftclient.Node{Type: "paragraph"}
		if line != "" {
			p.Content = []*riftclient.Node{{Type: "text", Text: line}}
		}
		doc.Content = append(doc.Content, p)
	}
	return doc
}

// expandCustomEmoji splits custom emoji tokens in text nodes into customEmoji nodes, keeping the
// text's marks. Code (inline marks and code blocks) is left as it is.
func expandCustomEmoji(n *riftclient.Node) {
	if n == nil || n.Type == "codeBlock" || n.Type == "code_block" {
		return
	}
	var out []*riftclient.Node
	for _, c := range n.Content {
		if c.Type != "text" || hasMark(c, "code") || !strings.Contains(c.Text, "<:") {
			expandCustomEmoji(c)
			out = append(out, c)
			continue
		}
		last := 0
		for _, m := range customEmojiInText.FindAllStringSubmatchIndex(c.Text, -1) {
			if m[0] > last {
				out = append(out, &riftclient.Node{Type: "text", Text: c.Text[last:m[0]], Marks: c.Marks})
			}
			out = append(out, &riftclient.Node{Type: "customEmoji", Marks: c.Marks, Attrs: map[string]any{
				"shortcode": c.Text[m[2]:m[3]], "r2Key": c.Text[m[4]:m[5]],
			}})
			last = m[1]
		}
		if last < len(c.Text) {
			out = append(out, &riftclient.Node{Type: "text", Text: c.Text[last:], Marks: c.Marks})
		}
	}
	n.Content = out
}

// withEmojiTokens returns the content text to send with an AST: when the AST carries custom emoji
// tokens (from Matrix emoticons), the Matrix body only has their ":shortcode:" alt text, so the
// text is rebuilt from the AST, as the web client's content holds the tokens.
func withEmojiTokens(text string, ast *riftclient.Node) string {
	if ast == nil {
		return text
	}
	if plain := ast.PlainText(); customEmojiInText.MatchString(plain) {
		return plain
	}
	return text
}

func hasMark(n *riftclient.Node, typ string) bool {
	for _, m := range n.Marks {
		if m.Type == typ {
			return true
		}
	}
	return false
}

// KV entries: R2 key -> mxc (so each emoji is uploaded once) and mxc -> token (so Matrix reactions
// and inline emoticons using a bridged emoji can be sent back to Rift).
func kvEmojiMXC(r2Key string) database.Key { return database.Key("rift_emoji_mxc:" + r2Key) }
func kvEmojiToken(mxc id.ContentURIString) database.Key {
	return database.Key("rift_emoji_token:" + string(mxc))
}

// customEmojiMXC returns the Matrix copy of a Rift custom emoji image, uploading it on first use.
func (rc *RiftClient) customEmojiMXC(ctx context.Context, shortcode, r2Key string) (id.ContentURIString, error) {
	kv := rc.main.Bridge.DB.KV
	if mxc := kv.Get(ctx, kvEmojiMXC(r2Key)); mxc != "" {
		return id.ContentURIString(mxc), nil
	}
	data, mimeType, err := rc.client.DownloadMedia(ctx, r2Key)
	if err != nil {
		return "", fmt.Errorf("failed to download custom emoji: %w", err)
	}
	mxc, _, err := rc.main.Bridge.Bot.UploadMedia(ctx, "", data, shortcode, mimeType)
	if err != nil {
		return "", fmt.Errorf("failed to upload custom emoji: %w", err)
	}
	kv.Set(ctx, kvEmojiMXC(r2Key), string(mxc))
	kv.Set(ctx, kvEmojiToken(mxc), customEmojiTokenFor(shortcode, r2Key))
	return mxc, nil
}

// riftReactionToMatrix converts a Rift reaction emoji. Custom emoji become an mxc key with the
// Beeper shortcode field (how Beeper and other bridges send image reactions); if the image can't
// be bridged, the reaction falls back to ":shortcode:". The emoji ID stays the Rift token, which is
// what Rift's unreact endpoint expects.
func (rc *RiftClient) riftReactionToMatrix(ctx context.Context, emoji string) (networkid.EmojiID, string, map[string]any) {
	shortcode, r2Key, ok := parseCustomEmoji(emoji)
	if !ok {
		return networkid.EmojiID(emoji), emoji, nil
	}
	extra := map[string]any{"com.beeper.reaction.shortcode": ":" + shortcode + ":"}
	mxc, err := rc.customEmojiMXC(ctx, shortcode, r2Key)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("emoji", emoji).Msg("Falling back to shortcode for custom emoji reaction")
		return networkid.EmojiID(emoji), ":" + shortcode + ":", extra
	}
	return networkid.EmojiID(emoji), string(mxc), extra
}

// matrixReactionToRift maps a Matrix reaction key to a Rift emoji. Image reactions only work for
// emoji that came from Rift.
func (rc *RiftClient) matrixReactionToRift(ctx context.Context, key string) (string, error) {
	if !strings.HasPrefix(key, "mxc://") {
		return key, nil
	}
	if token := rc.main.Bridge.DB.KV.Get(ctx, kvEmojiToken(id.ContentURIString(key))); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("custom emoji reactions from outside Rift aren't supported")
}
