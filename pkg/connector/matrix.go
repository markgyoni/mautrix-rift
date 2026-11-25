package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

var errDMKeysMissing = errors.New("log in with your Rift Key to send encrypted DMs")

func (rc *RiftClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	meta := msg.Portal.Metadata.(*PortalMetadata)
	text := msg.Content.Body
	var attachments []*riftclient.Attachment
	switch msg.Content.MsgType {
	case event.MsgText, event.MsgNotice:
	case event.MsgEmote:
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		att, err := rc.uploadMatrixMedia(ctx, msg, meta.ShardID) // ShardID is empty for DMs and Splinters
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, att)
		text = ""
		if msg.Content.FileName != "" && msg.Content.Body != msg.Content.FileName {
			text = msg.Content.Body // caption
		}
	default:
		return nil, bridgev2.ErrUnsupportedMessageType
	}
	var ast *riftclient.Node
	if text != "" {
		ast = rc.matrixToRift(ctx, msg.Content, text)
		text = withEmojiTokens(text, ast)
		if msg.Content.MsgType == event.MsgEmote {
			text = "*" + text + "*"
			ast = emoteAST(ast)
		}
	}
	replyTo := ""
	if msg.ReplyTo != nil {
		replyTo = string(msg.ReplyTo.ID)
	}
	threadID := ""
	if msg.ThreadRoot != nil {
		threadID = string(msg.ThreadRoot.ID)
	}
	// A top-level message sent into a Forum room creates a new forum post: first line is the title,
	// remaining lines are the body. A single-line message uses the text for both so neither is empty.
	var forumTitle string
	if meta.ChannelType == "forum" && threadID == "" {
		var body string
		if forumTitle, body = splitForumPost(text); body != "" && body != text {
			text = body
			ast = rc.matrixToRift(ctx, &event.MessageEventContent{MsgType: event.MsgText, Body: text}, text)
		}
		if forumTitle == "" && len(attachments) > 0 {
			forumTitle = firstNonEmpty(attachments[0].Filename, "Post")
		}
	}
	cid := uuid.NewString()
	msg.AddPendingToIgnore(networkid.TransactionID(cid))

	var sent *riftclient.Message
	var err error
	req := &riftclient.SendMessageRequest{
		Title:           forumTitle,
		Content:         text,
		Attachments:     attachments,
		ReplyToID:       replyTo,
		ThreadID:        threadID,
		ClientMessageID: cid,
	}
	if ast != nil {
		req.ContentAST, _ = json.Marshal(ast)
	}
	switch meta.Kind {
	case PortalKindDM:
		if rc.keys == nil {
			return nil, errDMKeysMissing
		}
		var peerKey string
		if peerKey, err = rc.dmPeerKey(ctx, msg.Portal); err != nil {
			return nil, err
		}
		sent, err = rc.client.SendDM(ctx, rc.keys, string(msg.Portal.ID), peerKey, cid, riftclient.EncodeDMPlaintext(text, ast), replyTo, threadID, attachments)
	case PortalKindSplinter:
		sent, err = rc.client.SendSplinterMessage(ctx, string(msg.Portal.ID), req)
	default:
		sent, err = rc.client.SendMessage(ctx, string(msg.Portal.ID), req)
	}
	if err != nil {
		return nil, err
	}
	ts := sent.CreatedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{
		ID:        networkid.MessageID(sent.ID),
		SenderID:  networkid.UserID(rc.login.ID),
		Timestamp: ts,
	}}, nil
}

func (rc *RiftClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if meta := msg.Portal.Metadata.(*PortalMetadata); meta.Kind == PortalKindDM {
		if rc.keys == nil {
			return errDMKeysMissing
		}
		dmID, msgID := string(msg.Portal.ID), string(msg.EditTarget.ID)
		orig, err := rc.client.GetDMMessage(ctx, dmID, msgID)
		if err != nil {
			return fmt.Errorf("failed to fetch original DM: %w", err)
		}
		peerKey, err := rc.dmPeerKey(ctx, msg.Portal)
		if err != nil {
			return err
		}
		ast := rc.matrixToRift(ctx, msg.Content, msg.Content.Body)
		plain := riftclient.EncodeDMPlaintext(withEmojiTokens(msg.Content.Body, ast), ast)
		return rc.client.EditDM(ctx, rc.keys, dmID, msgID, firstNonEmpty(orig.ClientMessageID, msgID), orig.ThreadID, peerKey, plain)
	}
	text := msg.Content.Body
	ast := rc.matrixToRift(ctx, msg.Content, text)
	text = withEmojiTokens(text, ast)
	meta := msg.Portal.Metadata.(*PortalMetadata)
	if meta.Kind == PortalKindSplinter {
		return rc.client.EditSplinterMessage(ctx, string(msg.Portal.ID), string(msg.EditTarget.ID), text, ast)
	}
	// A forum post shows in Matrix as "title, then body". The web client only ever edits the body
	// (PATCH {content, content_ast}), so drop the first line and keep Rift's title as it is.
	if meta.ChannelType == "forum" && msg.EditTarget.ThreadRoot == "" {
		if _, body := splitForumPost(text); body != text {
			text = body
			ast = rc.matrixToRift(ctx, &event.MessageEventContent{MsgType: event.MsgText, Body: text}, text)
		}
	}
	_, err := rc.client.EditMessage(ctx, string(msg.Portal.ID), string(msg.EditTarget.ID), text, ast)
	return err
}

func (rc *RiftClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if msg.Portal.Metadata.(*PortalMetadata).usesDMEndpoints() {
		return rc.client.DeleteDM(ctx, string(msg.Portal.ID), string(msg.TargetMessage.ID))
	}
	return rc.client.DeleteMessage(ctx, string(msg.Portal.ID), string(msg.TargetMessage.ID))
}

func (rc *RiftClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	emoji, err := rc.matrixReactionToRift(ctx, msg.Content.RelatesTo.Key)
	if err != nil {
		return bridgev2.MatrixReactionPreResponse{}, err
	}
	return bridgev2.MatrixReactionPreResponse{
		SenderID: networkid.UserID(rc.login.ID),
		EmojiID:  networkid.EmojiID(emoji),
		Emoji:    emoji,
	}, nil
}

func (rc *RiftClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	if msg.Portal.Metadata.(*PortalMetadata).usesDMEndpoints() {
		return nil, rc.client.AddDMReaction(ctx, string(msg.Portal.ID), string(msg.TargetMessage.ID), msg.PreHandleResp.Emoji)
	}
	return nil, rc.client.AddReaction(ctx, string(msg.TargetMessage.ID), msg.PreHandleResp.Emoji)
}

func (rc *RiftClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if msg.Portal.Metadata.(*PortalMetadata).usesDMEndpoints() {
		return rc.client.RemoveDMReaction(ctx, string(msg.Portal.ID), string(msg.TargetReaction.MessageID), string(msg.TargetReaction.EmojiID))
	}
	return rc.client.RemoveReaction(ctx, string(msg.TargetReaction.MessageID), string(msg.TargetReaction.EmojiID))
}

func (rc *RiftClient) uploadMatrixMedia(ctx context.Context, msg *bridgev2.MatrixMessage, shardID string) (*riftclient.Attachment, error) {
	data, err := rc.main.Bridge.Bot.DownloadMedia(ctx, msg.Content.URL, msg.Content.File)
	if err != nil {
		return nil, fmt.Errorf("failed to download media from Matrix: %w", err)
	}
	name := msg.Content.FileName
	if name == "" {
		name = msg.Content.Body
	}
	mimeType := "application/octet-stream"
	att := &riftclient.Attachment{}
	if info := msg.Content.Info; info != nil {
		if info.MimeType != "" {
			mimeType = info.MimeType
		}
		att.Width, att.Height = info.Width, info.Height
	}
	up, err := rc.client.UploadMedia(ctx, shardID, data, name, mimeType)
	if err != nil {
		return nil, fmt.Errorf("failed to upload media to Rift: %w", err)
	}
	up.Width, up.Height = att.Width, att.Height
	return up, nil
}

// splitForumPost turns a Matrix message into a forum post: the first line (without a leading markdown
// "#") is the title and the remaining lines are the body. A single-line message keeps its text as the
// body too, so neither is empty. Empty text gives an empty title.
func splitForumPost(text string) (title, body string) {
	first, rest, _ := strings.Cut(strings.TrimSpace(text), "\n")
	first = strings.TrimSpace(first)
	if first == "" {
		return "", text
	}
	title = strings.TrimSpace(strings.TrimLeft(first, "#"))
	if title == "" {
		title = first
	}
	if rest = strings.TrimSpace(rest); rest != "" {
		return title, rest
	}
	return title, text
}

// emoteAST italicises every text node, matching the "*text*" content sent for /me.
func emoteAST(doc *riftclient.Node) *riftclient.Node {
	var walk func(*riftclient.Node)
	walk = func(n *riftclient.Node) {
		if n.Type == "text" && !slices.ContainsFunc(n.Marks, func(m riftclient.Mark) bool { return m.Type == "code" }) {
			n.Marks = addMark(n.Marks, riftclient.Mark{Type: "italic"})
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(doc)
	return doc
}

// HandleMatrixReadReceipt acks the newest bridged message at or before the read point.
func (rc *RiftClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	target := receipt.ExactMessage
	if target == nil {
		var err error
		target, err = rc.main.Bridge.DB.Message.GetLastNonFakePartAtOrBeforeTime(ctx, receipt.Portal.PortalKey, receipt.ReadUpTo)
		if err != nil || target == nil {
			return err
		}
	}
	if receipt.Portal.Metadata.(*PortalMetadata).usesDMEndpoints() {
		return rc.client.AckDM(ctx, string(receipt.Portal.ID), string(target.ID))
	}
	return rc.client.AckChannel(ctx, string(receipt.Portal.ID), string(target.ID))
}
