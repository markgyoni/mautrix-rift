package connector

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

// DM contact keys. Rift DM keys derive from the Rift Key and never rotate, so like Rift's own apps
// the bridge pins each contact's key on first use (PortalMetadata.PeerIdentityKey) and says so in the
// room if Rift ever serves a different one. Incoming DMs must also be encrypted with the contact's
// published key; anything else (e.g. a message injected with a key Rift doesn't publish for them)
// is shown with a warning.

const (
	untrustedKeyPrefix = "⚠️ [Sent with an encryption key that isn't this contact's] "
	// minPeerKeyRefetch limits forced refetches (e.g. while backfilling old messages after a key change).
	minPeerKeyRefetch = time.Minute
)

type peerKey struct {
	key     string // lowercase hex X25519
	fetched time.Time
}

// peerIdentityKey returns a DM peer's X25519 key after checking it is signed by their account key.
// With refetch, a cached key older than minPeerKeyRefetch is fetched again.
func (rc *RiftClient) peerIdentityKey(ctx context.Context, userID string, refetch bool) (string, error) {
	rc.cacheLock.RLock()
	cached, ok := rc.peerKeys[userID]
	rc.cacheLock.RUnlock()
	if ok && (!refetch || time.Since(cached.fetched) < minPeerKeyRefetch) {
		return cached.key, nil
	}
	ident, err := rc.client.GetPeerIdentity(ctx, userID)
	if err != nil {
		return "", err
	}
	if ident.DMWriteVersion != riftclient.DMv4Version {
		return "", fmt.Errorf("peer uses unsupported DM version %d", ident.DMWriteVersion)
	}
	signPub, err1 := hex.DecodeString(ident.AccountSigningPublicKey)
	identKey, err2 := hex.DecodeString(ident.IdentityKey)
	sig, err3 := hex.DecodeString(ident.IdentityKeySignature)
	if errors.Join(err1, err2, err3) != nil || len(signPub) != ed25519.PublicKeySize || !ed25519.Verify(signPub, identKey, sig) {
		return "", errors.New("peer identity key signature is invalid")
	}
	key := strings.ToLower(ident.IdentityKey)
	rc.cacheLock.Lock()
	rc.peerKeys[userID] = peerKey{key: key, fetched: time.Now()}
	rc.cacheLock.Unlock()
	return key, nil
}

// dmPeerKey returns the key to encrypt to in a DM portal, checking it against the pinned key.
func (rc *RiftClient) dmPeerKey(ctx context.Context, portal *bridgev2.Portal) (string, error) {
	key, err := rc.peerIdentityKey(ctx, portal.Metadata.(*PortalMetadata).RecipientID, false)
	if err != nil {
		return "", err
	}
	rc.pinPeerKey(ctx, portal, key)
	return key, nil
}

// dmPayloadKeyTrusted reports whether a DM was encrypted with the contact's published key.
func (rc *RiftClient) dmPayloadKeyTrusted(ctx context.Context, portal *bridgev2.Portal, p *riftclient.E2EEPayload) bool {
	if portal == nil {
		return true
	}
	meta := portal.Metadata.(*PortalMetadata)
	if meta.Kind != PortalKindDM || meta.RecipientID == "" {
		return true
	}
	// Our own messages carry the contact's key as recipient, theirs as sender.
	used := strings.ToLower(p.SenderPublicKey)
	if strings.EqualFold(used, rc.keys.DMPublicHex()) {
		used = strings.ToLower(p.RecipientPublicKey)
	}
	if used == meta.PeerIdentityKey {
		return true
	}
	// Not the pinned key: either the contact's key changed or the message used a key Rift doesn't
	// publish for them. Ask Rift for the current key to tell the two apart.
	current, err := rc.peerIdentityKey(ctx, meta.RecipientID, true)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("peer_id", meta.RecipientID).Msg("Failed to fetch DM contact key")
		return meta.PeerIdentityKey == "" // nothing to compare against yet
	}
	rc.pinPeerKey(ctx, portal, current)
	if used != current {
		zerolog.Ctx(ctx).Warn().Str("peer_id", meta.RecipientID).Str("used_key", used).Str("published_key", current).
			Msg("DM encrypted with a key that isn't the contact's published key")
		return false
	}
	return true
}

// pinPeerKey records the contact's key on first use and warns in the room when it changes.
func (rc *RiftClient) pinPeerKey(ctx context.Context, portal *bridgev2.Portal, key string) {
	meta := portal.Metadata.(*PortalMetadata)
	if meta.PeerIdentityKey == key {
		return
	}
	old := meta.PeerIdentityKey
	meta.PeerIdentityKey = key
	if err := portal.Save(ctx); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to save pinned DM contact key")
	}
	if old == "" {
		return
	}
	zerolog.Ctx(ctx).Warn().Str("peer_id", meta.RecipientID).Str("old_key", old).Str("new_key", key).Msg("DM contact key changed")
	if portal.MXID == "" {
		return
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body: "⚠️ This contact's encryption key changed. Rift keys only change when someone resets their " +
			"Rift Key, so if you didn't expect this, compare safety numbers with them in the Rift app " +
			"(Verify encryption) before sending anything sensitive.",
	}
	if _, err := rc.main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{Parsed: content}, nil); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to send key change notice")
	}
}
