package riftclient

import (
	"encoding/json"
	"time"
)

type User struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
}

func (u *User) Name() string {
	if u.DisplayName != nil && *u.DisplayName != "" {
		return *u.DisplayName
	}
	return u.Username
}

type AuthResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         *User  `json:"user"`
	WSTicket     string `json:"ws_ticket,omitempty"` // only on /auth/refresh
}

type Shard struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	OwnerID     string  `json:"owner_id"`
	IconKey     *string `json:"icon_key"`
	Description *string `json:"description"`
}

type Channel struct {
	ID          string  `json:"id"`
	ShardID     string  `json:"shard_id"`
	ParentID    *string `json:"parent_id"`
	Name        string  `json:"name"`
	ChannelType string  `json:"channel_type"` // text, voice, gallery, polls, counting, forum, ...
	Topic       *string `json:"topic"`
	Position    int     `json:"position"`
	// LastMessageAt is in the Shard bootstrap (null for empty channels).
	LastMessageAt *time.Time `json:"last_message_at"`
}

// ShardMember is one entry of GET /shards/{id}/members/window.
type ShardMember struct {
	UserID      string   `json:"user_id"`
	Username    string   `json:"username"`
	Nickname    *string  `json:"nickname"` // per-Shard nickname
	DisplayName *string  `json:"display_name"`
	AvatarURL   *string  `json:"avatar_url"`
	Roles       []string `json:"roles"`
}

// MemberEvent is the payload of MEMBER_JOIN (a full member object) and MEMBER_LEAVE / MEMBER_REMOVE /
// MEMBER_BAN (at least shard_id and user_id). Field names come from the web bundle's handlers.
type MemberEvent struct {
	ShardID     string  `json:"shard_id"`
	UserID      string  `json:"user_id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
}

// ReadUpdate is CHANNEL_READ_UPDATE / DM_READ_UPDATE: the logged-in user's own read marker moved
// (on another device, or via our ack). Confirmed in captures.
type ReadUpdate struct {
	ChannelID         string    `json:"channel_id"`
	DMChannelID       string    `json:"dm_channel_id"`
	LastReadMessageID string    `json:"last_read_message_id"`
	LastReadAt        time.Time `json:"last_read_at"`
}

type membersWindow struct {
	Members          []*ShardMember `json:"members"`
	Truncated        bool           `json:"truncated"`
	TotalMemberCount int            `json:"total_member_count"`
	NextOffset       *int           `json:"next_offset"`
}

type ShardBootstrap struct {
	Channels []*Channel `json:"channels"`
}

type DMChannel struct {
	ID                string    `json:"id"`
	RecipientID       string    `json:"recipient_id"`
	RecipientUsername string    `json:"recipient_username"`
	RecipientAvatar   *string   `json:"recipient_avatar_url"`
	Status            string    `json:"status"`
	Kind              string    `json:"kind"`
	IsIncomingRequest bool      `json:"is_incoming_request"`
	CreatedAt         time.Time `json:"created_at"`
	LastActivityAt    time.Time `json:"last_activity_at"`
}

type MessageAuthor struct {
	DisplayName string `json:"display_name"`
}

type Attachment struct {
	R2Key      string `json:"r2_key"`
	Filename   string `json:"filename,omitempty"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	ThumbR2Key string `json:"thumb_r2_key,omitempty"`
}

// Message covers both Shard channel messages and DM messages.
type Message struct {
	ID              string          `json:"id"`
	ChannelID       string          `json:"channel_id,omitempty"`
	DMChannelID     string          `json:"dm_channel_id,omitempty"`
	AuthorID        string          `json:"author_id"`
	Author          *MessageAuthor  `json:"author,omitempty"`
	Content         string          `json:"content"`
	ContentAST      json.RawMessage `json:"content_ast,omitempty"`
	MessageKind     string          `json:"message_kind,omitempty"` // "e2ee" for DMs
	E2EEPayload     *E2EEPayload    `json:"e2ee_payload,omitempty"`
	Attachments     []*Attachment   `json:"attachments"`
	Reactions       []*Reaction     `json:"reactions,omitempty"`
	ReplyToID       *string         `json:"reply_to_id"`
	ThreadID        *string         `json:"thread_id"`
	ClientMessageID string          `json:"client_message_id"`
	CreatedAt       time.Time       `json:"created_at"`
	EditedAt        *time.Time      `json:"edited_at"`
}

// Reaction is the per-emoji summary on a message object. UserIDs may be a sample when
// SampleTruncated is set.
type Reaction struct {
	Emoji           string   `json:"emoji"`
	Count           int      `json:"count"`
	UserIDs         []string `json:"user_ids"`
	SampleTruncated bool     `json:"sample_truncated"`
}

// SendMessageRequest is the body of POST /messages/{channel} and /dms/{dm}/messages.
type SendMessageRequest struct {
	Content         string          `json:"content"`
	ContentAST      json.RawMessage `json:"content_ast,omitempty"`
	ReplyPing       bool            `json:"reply_ping"`
	ReplyToID       string          `json:"reply_to_id,omitempty"`
	ClientMessageID string          `json:"client_message_id"`
	Attachments     []*Attachment   `json:"attachments,omitempty"`
	MessageKind     string          `json:"message_kind,omitempty"`
	E2EEPayload     *E2EEPayload    `json:"e2ee_payload,omitempty"`
}

type PeerIdentity struct {
	UserID                  string `json:"user_id"`
	AccountSigningPublicKey string `json:"account_signing_public_key"`
	IdentityKey             string `json:"identity_key"`
	IdentityKeySignature    string `json:"identity_key_signature"`
	DMWriteVersion          int    `json:"dm_write_version"`
}

type Profile struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
	AvatarURL   *string `json:"avatar_url"`
}

func (p *Profile) Name() string {
	if p.DisplayName != nil && *p.DisplayName != "" {
		return *p.DisplayName
	}
	return p.Username
}

// Gateway payloads.

type ReactionEvent struct {
	ChannelID   string `json:"channel_id"`
	DMChannelID string `json:"dm_channel_id"`
	MessageID   string `json:"message_id"`
	UserID      string `json:"user_id"`
	Emoji       string `json:"emoji"`
}

type MessageDeleteEvent struct {
	ChannelID   string `json:"channel_id"`
	DMChannelID string `json:"dm_channel_id"`
	ID          string `json:"id"`         // MESSAGE_DELETE
	MessageID   string `json:"message_id"` // DM deletes (sent as DM_MESSAGE_CREATE)
	Deleted     bool   `json:"deleted"`
}

type TypingEvent struct {
	ChannelID   string `json:"channel_id"`
	DMChannelID string `json:"dm_channel_id"`
	UserID      string `json:"user_id"`
}
