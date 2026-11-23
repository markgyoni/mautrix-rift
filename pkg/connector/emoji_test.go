package connector

import "testing"

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
