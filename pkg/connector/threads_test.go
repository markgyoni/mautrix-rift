package connector

import (
	"encoding/json"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

func TestParseMessageEvent(t *testing.T) {
	ts := time.Date(2026, 11, 24, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		typ     string
		data    string
		wantMsg bool
		wantDel bool
	}{
		{"new message", "MESSAGE_CREATE",
			`{"id":"m1","channel_id":"c","content":"hi","edited_at":null,"thread_id":"root"}`, true, false},
		{"real channel edit", "MESSAGE_UPDATE",
			`{"id":"m1","channel_id":"c","content":"hi2","edited_at":"2026-11-24T15:59:59.5Z"}`, true, false},
		// Captured: replying in a thread re-sends the root with thread_reply_count bumped.
		{"thread bump, never edited", "MESSAGE_UPDATE",
			`{"id":"root","channel_id":"c","content":"root","edited_at":null,"thread_reply_count":1}`, false, false},
		{"thread bump, edited long ago", "MESSAGE_UPDATE",
			`{"id":"root","channel_id":"c","content":"root","edited_at":"2026-11-24T10:00:00Z","thread_reply_count":2}`, false, false},
		{"DM thread bump", "DM_MESSAGE_CREATE",
			`{"id":"root","dm_channel_id":"d","edited":true,"edited_at":null,"thread_updated":true,"thread_reply_count":1}`, false, false},
		{"DM thread read marker", "DM_MESSAGE_CREATE",
			`{"dm_channel_id":"d","thread_id":"root","thread_read":true}`, false, false},
		{"DM delete", "DM_MESSAGE_CREATE",
			`{"deleted":true,"message_id":"m1","dm_channel_id":"d","channel_id":"d","thread_id":null}`, false, true},
		{"DM edit", "DM_MESSAGE_CREATE",
			`{"id":"m1","dm_channel_id":"d","content":"x","edited":true,"edited_at":"2026-11-24T15:59:59Z"}`, true, false},
		{"no id", "MESSAGE_CREATE", `{"channel_id":"c"}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, del, err := parseMessageEvent(&riftclient.Event{Type: tt.typ, Timestamp: ts, Data: json.RawMessage(tt.data)})
			if err != nil {
				t.Fatal(err)
			}
			if (msg != nil) != tt.wantMsg || (del != nil) != tt.wantDel {
				t.Fatalf("got msg=%v del=%v, want msg=%v del=%v", msg != nil, del != nil, tt.wantMsg, tt.wantDel)
			}
		})
	}
}

func TestParseMessageEventThreadID(t *testing.T) {
	msg, _, err := parseMessageEvent(&riftclient.Event{Type: "DM_MESSAGE_CREATE", Data: json.RawMessage(
		`{"id":"m2","dm_channel_id":"d","content":"reply","thread_id":"root","thread_reply_count":0}`)})
	if err != nil || msg == nil || msg.ThreadID == nil || *msg.ThreadID != "root" {
		t.Fatalf("thread reply not parsed: %+v %v", msg, err)
	}
}

func TestIsFreshEdit(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	recent := now.Add(-2 * time.Second)
	if isFreshEdit(nil, now) {
		t.Error("nil edited_at is not an edit")
	}
	if isFreshEdit(&old, now) {
		t.Error("an hour-old edited_at is not a fresh edit")
	}
	if !isFreshEdit(&recent, now) {
		t.Error("a 2s-old edited_at is a fresh edit")
	}
	if !isFreshEdit(&old, time.Time{}) {
		t.Error("without an event timestamp, trust edited_at")
	}
}

func TestSplitForumPost(t *testing.T) {
	tests := []struct{ in, title, body string }{
		{"Title\nBody line 1\nBody line 2", "Title", "Body line 1\nBody line 2"},
		{"# Heading title\n\nbody", "Heading title", "body"},
		{"just one line", "just one line", "just one line"},
		{"  padded  \n  body  ", "padded", "body"},
		{"###\nbody", "###", "body"},
		{"", "", ""},
	}
	for _, tt := range tests {
		title, body := splitForumPost(tt.in)
		if title != tt.title || body != tt.body {
			t.Errorf("splitForumPost(%q) = %q, %q; want %q, %q", tt.in, title, body, tt.title, tt.body)
		}
	}
}

func TestPrependForumTitle(t *testing.T) {
	c := &event.MessageEventContent{MsgType: event.MsgText, Body: "a <b>\nc"}
	prependForumTitle(c, "T&T")
	if c.Body != "T&T\n\na <b>\nc" {
		t.Errorf("body = %q", c.Body)
	}
	if c.FormattedBody != "<h3>T&amp;T</h3>a &lt;b&gt;<br>c" {
		t.Errorf("formatted = %q", c.FormattedBody)
	}

	c = &event.MessageEventContent{MsgType: event.MsgText, Body: "x", Format: event.FormatHTML, FormattedBody: "<strong>x</strong>"}
	prependForumTitle(c, "T")
	if c.FormattedBody != "<h3>T</h3><strong>x</strong>" {
		t.Errorf("formatted = %q", c.FormattedBody)
	}

	c = &event.MessageEventContent{MsgType: event.MsgText}
	prependForumTitle(c, "Only title")
	if c.Body != "Only title" || c.FormattedBody != "<h3>Only title</h3>" {
		t.Errorf("title-only post = %q / %q", c.Body, c.FormattedBody)
	}
}
