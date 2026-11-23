package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
		att, err := rc.uploadMatrixMedia(ctx, msg, meta.ShardID) // ShardID is empty for DMs
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
		if msg.Content.MsgType == event.MsgEmote {
			text = "*" + text + "*"
			ast = emoteAST(ast)
		}
	}
	replyTo := ""
	if msg.ReplyTo != nil {
		replyTo = string(msg.ReplyTo.ID)
	}
	cid := uuid.NewString()
	msg.AddPendingToIgnore(networkid.TransactionID(cid))

	var sent *riftclient.Message
	var err error
	if meta.Kind == PortalKindDM {
		if rc.keys == nil {
			return nil, errDMKeysMissing
		}
		var peerKey string
		if peerKey, err = rc.dmPeerKey(ctx, msg.Portal); err != nil {
			return nil, err
		}
		sent, err = rc.client.SendDM(ctx, rc.keys, string(msg.Portal.ID), peerKey, riftclient.EncodeDMPlaintext(text, ast), replyTo, attachments)
	} else {
		req := &riftclient.SendMessageRequest{
			Content:         text,
			Attachments:     attachments,
			ReplyToID:       replyTo,
			ClientMessageID: cid,
		}
		if ast != nil {
			req.ContentAST, _ = json.Marshal(ast)
		}
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
		plain := riftclient.EncodeDMPlaintext(msg.Content.Body, rc.matrixToRift(ctx, msg.Content, msg.Content.Body))
		return rc.client.EditDM(ctx, rc.keys, dmID, msgID, firstNonEmpty(orig.ClientMessageID, msgID), peerKey, plain)
	}
	_, err := rc.client.EditMessage(ctx, string(msg.Portal.ID), string(msg.EditTarget.ID), msg.Content.Body, rc.matrixToRift(ctx, msg.Content, msg.Content.Body))
	return err
}

func (rc *RiftClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if msg.Portal.Metadata.(*PortalMetadata).Kind == PortalKindDM {
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
	if msg.Portal.Metadata.(*PortalMetadata).Kind == PortalKindDM {
		return nil, rc.client.AddDMReaction(ctx, string(msg.Portal.ID), string(msg.TargetMessage.ID), msg.PreHandleResp.Emoji)
	}
	return nil, rc.client.AddReaction(ctx, string(msg.TargetMessage.ID), msg.PreHandleResp.Emoji)
}

func (rc *RiftClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if msg.Portal.Metadata.(*PortalMetadata).Kind == PortalKindDM {
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
	if receipt.Portal.Metadata.(*PortalMetadata).Kind == PortalKindDM {
		return rc.client.AckDM(ctx, string(receipt.Portal.ID), string(target.ID))
	}
	return rc.client.AckChannel(ctx, string(receipt.Portal.ID), string(target.ID))
}
