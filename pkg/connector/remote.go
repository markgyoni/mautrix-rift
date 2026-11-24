package connector

import (
	"context"
	"encoding/json"
	"html"
	"path"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

// handleEvent converts gateway events into bridgev2 remote events.
func (rc *RiftClient) handleEvent(ctx context.Context, evt *riftclient.Event) {
	log := zerolog.Ctx(ctx).With().Str("event_type", evt.Type).Int64("seq", evt.Seq).Logger()
	// Payloads are logged so unknown shapes can be reverse-engineered (DM text stays encrypted).
	log.Debug().RawJSON("data", evt.Data).Msg("Rift gateway event")
	switch evt.Type {
	case "MESSAGE_CREATE", "MESSAGE_UPDATE", "DM_MESSAGE_CREATE", "DM_MESSAGE_UPDATE":
		msg, del, err := parseMessageEvent(evt)
		switch {
		case err != nil:
			log.Err(err).Msg("Failed to parse message")
		case del != nil:
			rc.queueDelete(evt, del)
		case msg != nil:
			rc.queueMessage(evt.Type, msg)
		}
	case "FORUM_POST_CREATE", "THREAD_READ_UPDATE":
		// FORUM_POST_CREATE duplicates MESSAGE_CREATE; THREAD_READ_UPDATE is thread-scoped so shouldn't
		// move the main room's read receipt.
		return
	case "MESSAGE_DELETE", "DM_MESSAGE_DELETE":
		var del riftclient.MessageDeleteEvent
		if err := json.Unmarshal(evt.Data, &del); err != nil {
			log.Err(err).Msg("Failed to parse delete")
			return
		}
		rc.queueDelete(evt, &del)
	case "REACTION_ADD", "REACTION_REMOVE", "DM_REACTION_ADD", "DM_REACTION_REMOVE":
		var r riftclient.ReactionEvent
		if err := json.Unmarshal(evt.Data, &r); err != nil {
			log.Err(err).Msg("Failed to parse reaction")
			return
		}
		typ := bridgev2.RemoteEventReaction
		var emojiID networkid.EmojiID
		var emoji string
		var extra map[string]any
		if strings.HasSuffix(evt.Type, "REACTION_REMOVE") {
			typ = bridgev2.RemoteEventReactionRemove
			emojiID = networkid.EmojiID(r.Emoji)
		} else {
			emojiID, emoji, extra = rc.riftReactionToMatrix(ctx, r.Emoji)
		}
		rc.login.QueueRemoteEvent(&simplevent.Reaction{
			EventMeta: simplevent.EventMeta{
				Type: typ, PortalKey: rc.portalKeyFor(firstNonEmpty(r.DMChannelID, r.ChannelID)), Timestamp: evt.Timestamp,
				Sender: rc.sender(r.UserID),
			},
			TargetMessage: networkid.MessageID(r.MessageID),
			EmojiID:       emojiID,
			Emoji:         emoji,
			ExtraContent:  extra,
		})
	case "TYPING_START", "DM_TYPING_START":
		var t riftclient.TypingEvent
		if err := json.Unmarshal(evt.Data, &t); err != nil || t.UserID == string(rc.login.ID) {
			return
		}
		chatID := t.ChannelID
		if chatID == "" {
			chatID = t.DMChannelID
		}
		rc.login.QueueRemoteEvent(&simplevent.Typing{
			EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventTyping, PortalKey: rc.portalKeyFor(chatID), Sender: rc.sender(t.UserID)},
			Timeout:   10 * time.Second,
		})
	case "DM_CHANNEL_CREATE":
		var dm riftclient.DMChannel
		if err := json.Unmarshal(evt.Data, &dm); err == nil {
			rc.cacheLock.Lock()
			rc.dms[dm.ID] = &dm
			rc.cacheLock.Unlock()
		}
	case "CHANNEL_CREATE", "CHANNEL_UPDATE":
		var ch riftclient.Channel
		if err := json.Unmarshal(evt.Data, &ch); err == nil && ch.ID != "" {
			rc.cacheLock.Lock()
			rc.channels[ch.ID] = &ch
			rc.cacheLock.Unlock()
			if bridgedChannelTypes[ch.ChannelType] {
				rc.login.QueueRemoteEvent(&simplevent.ChatResync{
					EventMeta: simplevent.EventMeta{
						Type:         bridgev2.RemoteEventChatResync,
						PortalKey:    channelPortalKey(ch.ID, rc.login),
						CreatePortal: true,
						Timestamp:    evt.Timestamp,
					},
					GetChatInfoFunc: rc.GetChatInfo,
				})
			}
		}
	case "CHANNEL_DELETE":
		var ch struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(evt.Data, &ch); err == nil && ch.ID != "" {
			rc.cacheLock.Lock()
			delete(rc.channels, ch.ID)
			rc.cacheLock.Unlock()
			rc.login.QueueRemoteEvent(&simplevent.ChatDelete{
				EventMeta: simplevent.EventMeta{
					Type:      bridgev2.RemoteEventChatDelete,
					PortalKey: channelPortalKey(ch.ID, rc.login),
					Timestamp: evt.Timestamp,
				},
			})
		}
	case "SHARD_CREATE", "SHARD_UPDATE":
		var s riftclient.Shard
		if err := json.Unmarshal(evt.Data, &s); err != nil || s.ID == "" || s.Name == "" {
			return
		}
		rc.cacheLock.Lock()
		_, known := rc.shards[s.ID]
		if known {
			rc.shards[s.ID] = &s
		}
		rc.cacheLock.Unlock()
		if known {
			rc.queueResync(shardPortalKey(s.ID, rc.login), time.Time{}) // name/icon/description changes
		} else {
			go rc.joinShard(ctx, &s)
		}
	case "CHANNEL_READ_UPDATE", "DM_READ_UPDATE":
		var r riftclient.ReadUpdate
		if err := json.Unmarshal(evt.Data, &r); err != nil || r.LastReadMessageID == "" {
			return
		}
		rc.login.QueueRemoteEvent(&simplevent.Receipt{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventReadReceipt, PortalKey: rc.portalKeyFor(firstNonEmpty(r.DMChannelID, r.ChannelID)),
				Sender: rc.selfSender(), Timestamp: r.LastReadAt,
			},
			LastTarget: networkid.MessageID(r.LastReadMessageID),
			ReadUpTo:   r.LastReadAt,
		})
	case "SPLINTER_UPDATE":
		var u riftclient.SplinterUpdate
		if err := json.Unmarshal(evt.Data, &u); err != nil || u.ID == "" {
			return
		}
		rc.handleSplinterUpdate(ctx, evt, &u)
	case "MEMBER_JOIN", "MEMBER_LEAVE", "MEMBER_REMOVE", "MEMBER_BAN":
		var m riftclient.MemberEvent
		if err := json.Unmarshal(evt.Data, &m); err != nil || m.ShardID == "" || m.UserID == "" {
			return
		}
		rc.queueMemberChange(evt, &m)
	default:
		log.Trace().RawJSON("data", evt.Data).Msg("Unhandled gateway event")
	}
}

// parseMessageEvent sorts a (DM_)MESSAGE_CREATE/UPDATE payload into a message to bridge, a delete, or
// nothing (both nil) for the non-message events Rift sends under these types (confirmed in captures):
//   - DM deletes: DM_MESSAGE_CREATE {"deleted": true, "message_id", "dm_channel_id", ...}
//   - DM thread read markers: DM_MESSAGE_CREATE {"dm_channel_id", "thread_id", "thread_read": true}
//   - thread reply count bumps on the root message: DM_MESSAGE_CREATE {"thread_updated": true, ...} and
//     MESSAGE_UPDATE with the root's old edited_at (null if never edited). Only an edited_at close to
//     the event time is a real edit.
func parseMessageEvent(evt *riftclient.Event) (*riftclient.Message, *riftclient.MessageDeleteEvent, error) {
	var flags struct {
		Deleted       bool `json:"deleted"`
		ThreadRead    bool `json:"thread_read"`
		ThreadUpdated bool `json:"thread_updated"`
	}
	if err := json.Unmarshal(evt.Data, &flags); err != nil {
		return nil, nil, err
	}
	if flags.Deleted {
		var del riftclient.MessageDeleteEvent
		if err := json.Unmarshal(evt.Data, &del); err != nil {
			return nil, nil, err
		}
		return nil, &del, nil
	}
	if flags.ThreadRead || flags.ThreadUpdated {
		return nil, nil, nil
	}
	var msg riftclient.Message
	if err := json.Unmarshal(evt.Data, &msg); err != nil {
		return nil, nil, err
	}
	if msg.ID == "" {
		return nil, nil, nil
	}
	if (evt.Type == "MESSAGE_UPDATE" || evt.Type == "DM_MESSAGE_UPDATE") && !isFreshEdit(msg.EditedAt, evt.Timestamp) {
		return nil, nil, nil
	}
	return &msg, nil, nil
}

func (rc *RiftClient) sender(userID string) bridgev2.EventSender {
	if userID == string(rc.login.ID) {
		return rc.selfSender()
	}
	return bridgev2.EventSender{Sender: networkid.UserID(userID)}
}

// portalKeyFor maps a Rift channel, DM or Splinter ID to its portal key.
func (rc *RiftClient) portalKeyFor(chatID string) networkid.PortalKey {
	rc.cacheLock.RLock()
	_, isDM := rc.dms[chatID]
	rc.cacheLock.RUnlock()
	if isDM {
		return dmPortalKey(chatID, rc.login)
	}
	return channelPortalKey(chatID, rc.login)
}

func (rc *RiftClient) queueMessage(evtType string, msg *riftclient.Message) {
	chatID := msg.ChannelID
	if msg.DMChannelID != "" {
		chatID = msg.DMChannelID
	}
	ts := msg.CreatedAt
	typ := bridgev2.RemoteEventMessage
	// The web client only listens for DM_MESSAGE_CREATE, so DM edits may arrive as a re-sent create.
	if evtType == "MESSAGE_UPDATE" || evtType == "DM_MESSAGE_UPDATE" || (evtType == "DM_MESSAGE_CREATE" && msg.EditedAt != nil) {
		typ = bridgev2.RemoteEventEdit
		if msg.EditedAt != nil {
			ts = *msg.EditedAt
		}
	}
	rc.login.QueueRemoteEvent(&simplevent.Message[*riftclient.Message]{
		EventMeta: simplevent.EventMeta{
			Type:         typ,
			PortalKey:    rc.portalKeyFor(chatID),
			Sender:       rc.sender(msg.AuthorID),
			CreatePortal: true,
			Timestamp:    ts,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("message_id", msg.ID)
			},
		},
		Data:               msg,
		ID:                 networkid.MessageID(msg.ID),
		TargetMessage:      networkid.MessageID(msg.ID),
		TransactionID:      networkid.TransactionID(msg.ClientMessageID),
		ConvertMessageFunc: rc.convertMessage,
		ConvertEditFunc:    rc.convertEdit,
	})
}

// messageContent returns a message's plain text and content_ast, decrypting DMs.
func (rc *RiftClient) messageContent(ctx context.Context, portal *bridgev2.Portal, msg *riftclient.Message) (string, *riftclient.Node) {
	if msg.MessageKind != "e2ee" || msg.E2EEPayload == nil {
		return msg.Content, riftclient.ParseAST(msg.ContentAST)
	}
	if rc.keys == nil {
		return "[Encrypted DM: log in with your Rift Key to read it]", nil
	}
	plain, err := riftclient.DecryptDMv4(rc.keys.DMPrivate, msg.E2EEPayload, msg.ClientMessageID, msg.ThreadID)
	if err != nil {
		rc.login.Log.Err(err).Str("message_id", msg.ID).Msg("Failed to decrypt DM")
		return "[Failed to decrypt DM]", nil
	}
	text, ast := riftclient.DecodeDMPlaintext(plain)
	if !rc.dmPayloadKeyTrusted(ctx, portal, msg.E2EEPayload) {
		// Shown as plain text so the warning can't be lost in formatting.
		return untrustedKeyPrefix + text, nil
	}
	return text, ast
}

func (rc *RiftClient) convertText(ctx context.Context, portal *bridgev2.Portal, msg *riftclient.Message) *bridgev2.ConvertedMessagePart {
	text, ast := rc.messageContent(ctx, portal, msg)
	content := rc.astToMatrix(ctx, ast, text)
	if msg.Title != nil && strings.TrimSpace(*msg.Title) != "" {
		prependForumTitle(content, strings.TrimSpace(*msg.Title))
	}
	if strings.TrimSpace(content.Body) == "" {
		return nil
	}
	return &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: content}
}

// prependForumTitle formats a forum post's title above its body (plain text and HTML).
func prependForumTitle(content *event.MessageEventContent, title string) {
	body := strings.TrimSpace(content.Body)
	if body == "" {
		content.Body = title
		content.Format = event.FormatHTML
		content.FormattedBody = "<h3>" + html.EscapeString(title) + "</h3>"
		return
	}
	bodyHTML := content.FormattedBody
	if content.Format != event.FormatHTML || bodyHTML == "" {
		bodyHTML = strings.ReplaceAll(html.EscapeString(content.Body), "\n", "<br>")
	}
	content.Body = title + "\n\n" + content.Body
	content.Format = event.FormatHTML
	content.FormattedBody = "<h3>" + html.EscapeString(title) + "</h3>" + bodyHTML
}

func (rc *RiftClient) convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, att *riftclient.Attachment) *bridgev2.ConvertedMessagePart {
	name := att.Filename
	if name == "" {
		name = path.Base(att.R2Key)
	}
	part := &bridgev2.ConvertedMessagePart{ID: networkid.PartID(att.R2Key), Type: event.EventMessage}
	data, mimeType, err := rc.client.DownloadMedia(ctx, att.R2Key)
	if err == nil {
		if att.MimeType != "" {
			mimeType = att.MimeType
		}
		var content event.MessageEventContent
		content.URL, content.File, err = intent.UploadMedia(ctx, portal.MXID, data, name, mimeType)
		if err == nil {
			content.Body = name
			content.Info = &event.FileInfo{MimeType: mimeType, Size: len(data), Width: att.Width, Height: att.Height}
			switch {
			case strings.HasPrefix(mimeType, "image/"):
				content.MsgType = event.MsgImage
			case strings.HasPrefix(mimeType, "video/"):
				content.MsgType = event.MsgVideo
			case strings.HasPrefix(mimeType, "audio/"):
				content.MsgType = event.MsgAudio
			default:
				content.MsgType = event.MsgFile
			}
			part.Content = &content
			return part
		}
	}
	zerolog.Ctx(ctx).Err(err).Str("r2_key", att.R2Key).Msg("Failed to bridge attachment")
	part.Content = &event.MessageEventContent{MsgType: event.MsgNotice, Body: "[Failed to bridge attachment: " + name + "]"}
	return part
}

func (rc *RiftClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *riftclient.Message) (*bridgev2.ConvertedMessage, error) {
	cm := &bridgev2.ConvertedMessage{}
	if text := rc.convertText(ctx, portal, msg); text != nil {
		cm.Parts = append(cm.Parts, text)
	}
	for _, att := range msg.Attachments {
		cm.Parts = append(cm.Parts, rc.convertAttachment(ctx, portal, intent, att))
	}
	// On shutdown, fail instead of sending "[Failed to bridge attachment]" placeholders for good media.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if msg.ReplyToID != nil && *msg.ReplyToID != "" {
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: networkid.MessageID(*msg.ReplyToID)}
	}
	if msg.ThreadID != nil && *msg.ThreadID != "" {
		tid := networkid.MessageID(*msg.ThreadID)
		cm.ThreadRoot = &tid
	}
	return cm, nil
}

func (rc *RiftClient) convertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, msg *riftclient.Message) (*bridgev2.ConvertedEdit, error) {
	// Only the text part can be edited; attachments keep their original parts.
	text := rc.convertText(ctx, portal, msg)
	if text == nil || len(existing) == 0 || existing[0].ID == "" || existing[0].PartID != "" {
		return &bridgev2.ConvertedEdit{}, nil
	}
	return &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{{
		Part:    existing[0],
		Type:    text.Type,
		Content: text.Content,
	}}}, nil
}

func (rc *RiftClient) queueDelete(evt *riftclient.Event, del *riftclient.MessageDeleteEvent) {
	rc.login.QueueRemoteEvent(&simplevent.MessageRemove{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessageRemove,
			PortalKey: rc.portalKeyFor(firstNonEmpty(del.DMChannelID, del.ChannelID)),
			Timestamp: evt.Timestamp,
		},
		TargetMessage: networkid.MessageID(firstNonEmpty(del.ID, del.MessageID)),
	})
}

// queueMemberChange applies a Shard join/leave to every bridged channel room of that Shard.
func (rc *RiftClient) queueMemberChange(evt *riftclient.Event, m *riftclient.MemberEvent) {
	if m.UserID == string(rc.login.ID) {
		return // TODO: leaving a Shard should clean up its portals
	}
	rc.membersLock.Lock()
	delete(rc.shardMembers, m.ShardID)
	rc.membersLock.Unlock()

	member := bridgev2.ChatMember{EventSender: rc.sender(m.UserID), Membership: event.MembershipJoin}
	switch evt.Type {
	case "MEMBER_LEAVE", "MEMBER_REMOVE":
		member.Membership = event.MembershipLeave
	case "MEMBER_BAN":
		member.Membership = event.MembershipBan
	}
	if evt.Type == "MEMBER_JOIN" && m.Username != "" {
		name := firstNonEmpty(derefStr(m.DisplayName), m.Username)
		member.UserInfo = &bridgev2.UserInfo{Name: &name, Identifiers: []string{"rift:" + m.Username}}
	}
	rc.cacheLock.RLock()
	var keys []networkid.PortalKey
	for id, ch := range rc.channels {
		if ch.ShardID == m.ShardID && bridgedChannelTypes[ch.ChannelType] {
			keys = append(keys, channelPortalKey(id, rc.login))
		}
	}
	rc.cacheLock.RUnlock()
	for _, key := range keys {
		rc.login.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatInfoChange, PortalKey: key, Timestamp: evt.Timestamp},
			ChatInfoChange: &bridgev2.ChatInfoChange{
				MemberChanges: &bridgev2.ChatMemberList{Members: []bridgev2.ChatMember{member}},
			},
		})
	}
}

// handleSplinterUpdate mirrors the web client: a deleted Splinter, or one the user left or was removed
// from, goes away; an update carrying the name and member list replaces the cached Splinter. Anything
// else (a partial payload) triggers a re-list. The payload shape comes from the web bundle.
func (rc *RiftClient) handleSplinterUpdate(ctx context.Context, evt *riftclient.Event, u *riftclient.SplinterUpdate) {
	key := splinterPortalKey(u.ID, rc.login)
	selfRemoved := u.RemovedUserID == string(rc.login.ID) && (u.Action == "member_removed" || u.Action == "member_left")
	if u.Deleted || selfRemoved {
		rc.cacheLock.Lock()
		delete(rc.splinters, u.ID)
		rc.cacheLock.Unlock()
		rc.login.QueueRemoteEvent(&simplevent.ChatDelete{
			EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatDelete, PortalKey: key, Timestamp: evt.Timestamp},
		})
		return
	}
	if u.Name != "" && u.Members != nil {
		s := u.Splinter
		rc.cacheLock.Lock()
		if old, ok := rc.splinters[u.ID]; ok && s.LastActivityAt.IsZero() {
			s.LastActivityAt = old.LastActivityAt
		}
		rc.splinters[u.ID] = &s
		rc.cacheLock.Unlock()
	} else if err := rc.refreshSplinters(ctx); err != nil {
		zerolog.Ctx(ctx).Err(err).Str("splinter_id", u.ID).Msg("Failed to refresh Splinters after update")
		return
	}
	rc.login.QueueRemoteEvent(&simplevent.ChatResync{
		EventMeta:       simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: key, CreatePortal: true, Timestamp: evt.Timestamp},
		GetChatInfoFunc: rc.GetChatInfo,
	})
}

// isFreshEdit reports whether editedAt belongs to the update event at evtTS (rather than an earlier edit
// repeated in an unrelated update). A missing event timestamp falls back to trusting editedAt.
func isFreshEdit(editedAt *time.Time, evtTS time.Time) bool {
	if editedAt == nil {
		return false
	}
	if evtTS.IsZero() {
		return true
	}
	d := evtTS.Sub(*editedAt)
	return d > -time.Minute && d < time.Minute
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
