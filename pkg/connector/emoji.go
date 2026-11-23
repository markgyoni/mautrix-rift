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
