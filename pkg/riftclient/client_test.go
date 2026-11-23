package riftclient

import "testing"

func TestMediaKey(t *testing.T) {
	cases := map[string]string{
		"https://api.therift.chat/media-svc/uploads/u/profile-derived/a_avatar_256.jpg": "uploads/u/profile-derived/a_avatar_256.jpg",
		"https://api.therift.chat/media-svc/uploads/shared/abc?x=1":                     "uploads/shared/abc",
		"uploads/u/a.jpg": "uploads/u/a.jpg",
		"https://media.therift.chat/uploads/u/a.jpg.1795357054.sig": "https://media.therift.chat/uploads/u/a.jpg.1795357054.sig",
	}
	for in, want := range cases {
		if got := MediaKey(in); got != want {
			t.Errorf("MediaKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	e := &HTTPError{Status: 429, Body: `{"error":{"code":"RATE_LIMITED","message":"rate limited, retry after 3 seconds","retry_after_seconds":3}}`}
	if got := e.RetryAfter(); got != 3 {
		t.Errorf("RetryAfter = %d, want 3", got)
	}
	if got := (&HTTPError{Status: 429, Body: "nope"}).RetryAfter(); got != 1 {
		t.Errorf("RetryAfter fallback = %d, want 1", got)
	}
}
