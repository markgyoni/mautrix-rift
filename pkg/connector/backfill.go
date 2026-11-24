package connector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

var _ bridgev2.BackfillingNetworkAPI = (*RiftClient)(nil)

// maxHistoryPage caps one history request; the web client asks for 40-50.
const maxHistoryPage = 50

func (rc *RiftClient) fetchHistory(ctx context.Context, portal *bridgev2.Portal, p riftclient.HistoryParams) ([]*riftclient.Message, error) {
	if portal.Metadata.(*PortalMetadata).usesDMEndpoints() {
		return rc.client.GetDMMessages(ctx, string(portal.ID), p)
	}
	return rc.client.GetMessages(ctx, string(portal.ID), p)
}

// History cursors are "<created_at RFC3339Nano>|<message id>" of the oldest message returned.
func historyCursor(m *riftclient.Message) networkid.PaginationCursor {
	return networkid.PaginationCursor(m.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + m.ID)
}

func parseHistoryCursor(c networkid.PaginationCursor) (time.Time, string, error) {
	ts, msgID, ok := strings.Cut(string(c), "|")
	if !ok {
		return time.Time{}, "", fmt.Errorf("invalid history cursor %q", c)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return t, msgID, err
}

// FetchMessages pages history with before/before_id (newest first, confirmed in captures).
// Forward fetches (catch-up after downtime) page back from the newest message until they reach the
// anchor, so they only rely on the confirmed "before" semantics.
func (rc *RiftClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	if params.ThreadRoot != "" {
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}
	pageSize := min(max(params.Count, 1), maxHistoryPage)
	log := zerolog.Ctx(ctx)
	var msgs []*riftclient.Message
	resp := &bridgev2.FetchMessagesResponse{Forward: params.Forward}

	if params.Forward {
		anchor := params.AnchorMessage
		p := riftclient.HistoryParams{Limit: pageSize}
	pages:
		for len(msgs) < params.Count {
			page, err := rc.fetchHistory(ctx, params.Portal, p)
			if err != nil {
				return nil, err
			}
			for _, m := range page {
				if anchor != nil && (m.ID == string(anchor.ID) || !m.CreatedAt.After(anchor.Timestamp)) {
					break pages
				}
				msgs = append(msgs, m)
			}
			if len(page) < pageSize {
				break
			}
			last := page[len(page)-1]
			p.Before, p.BeforeID = last.CreatedAt, last.ID
		}
		if len(msgs) > params.Count {
			msgs = msgs[:params.Count]
		}
		resp.MarkRead = anchor == nil
	} else {
		p := riftclient.HistoryParams{Limit: pageSize}
		switch {
		case params.Cursor != "":
			var err error
			if p.Before, p.BeforeID, err = parseHistoryCursor(params.Cursor); err != nil {
				return nil, err
			}
		case params.AnchorMessage != nil:
			p.Before, p.BeforeID = params.AnchorMessage.Timestamp, string(params.AnchorMessage.ID)
		}
		page, err := rc.fetchHistory(ctx, params.Portal, p)
		if err != nil {
			return nil, err
		}
		msgs = page
		resp.HasMore = len(page) == pageSize
		if len(page) > 0 {
			resp.Cursor = historyCursor(page[len(page)-1])
		}
	}
	log.Debug().Bool("forward", params.Forward).Int("count", len(msgs)).Msg("Fetched Rift history")

	// Rift returns newest first; bridgev2 wants oldest first.
	slices.SortStableFunc(msgs, func(a, b *riftclient.Message) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, m := range msgs {
		if bm := rc.backfillMessage(ctx, params.Portal, m); bm != nil {
			resp.Messages = append(resp.Messages, bm)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err // stopped mid-batch: retry the whole batch later
	}
	return resp, nil
}

func (rc *RiftClient) backfillMessage(ctx context.Context, portal *bridgev2.Portal, m *riftclient.Message) *bridgev2.BackfillMessage {
	sender := rc.sender(m.AuthorID)
	intent, ok := portal.GetIntentFor(ctx, sender, rc.login, bridgev2.RemoteEventMessage)
	if !ok {
		return nil
	}
	converted, err := rc.convertMessage(ctx, portal, intent, m)
	if err != nil || len(converted.Parts) == 0 {
		return nil
	}
	bm := &bridgev2.BackfillMessage{
		ConvertedMessage: converted,
		Sender:           sender,
		ID:               networkid.MessageID(m.ID),
		TxnID:            networkid.TransactionID(m.ClientMessageID),
		Timestamp:        m.CreatedAt,
	}
	for _, r := range m.Reactions {
		emojiID, emoji, extra := rc.riftReactionToMatrix(ctx, r.Emoji)
		for _, uid := range r.UserIDs {
			bm.Reactions = append(bm.Reactions, &bridgev2.BackfillReaction{
				Sender:       rc.sender(uid),
				EmojiID:      emojiID,
				Emoji:        emoji,
				ExtraContent: extra,
			})
		}
	}
	return bm
}
