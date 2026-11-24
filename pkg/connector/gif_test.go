package connector

import "testing"

func TestSplitGIFs(t *testing.T) {
	rest, gifs := splitGIFs("![GIF|220x183](https://static.klipy.com/ii/4493/95/9d/Bz2yGlEhtfYgF.webp)")
	if rest != "" || len(gifs) != 1 || gifs[0].Width != 220 || gifs[0].Height != 183 || gifs[0].URL != "https://static.klipy.com/ii/4493/95/9d/Bz2yGlEhtfYgF.webp" {
		t.Fatalf("got %q %+v", rest, gifs)
	}
	rest, gifs = splitGIFs("lol ![GIF](https://static.klipy.com/a.webp)")
	if rest != "lol" || len(gifs) != 1 || gifs[0].Width != 0 {
		t.Fatalf("got %q %+v", rest, gifs)
	}
	for _, s := range []string{"![x](https://evil.example/a.png)", "![x](https://klipy.com.evil.example/a.png)", "plain text"} {
		if rest, gifs := splitGIFs(s); rest != s || gifs != nil {
			t.Fatalf("%q: got %q %+v", s, rest, gifs)
		}
	}
}
