package connector

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

// RiftClient is the bridgev2.NetworkAPI for one logged-in Rift account.
type RiftClient struct {
	main   *RiftConnector
	login  *bridgev2.UserLogin
	client *riftclient.Client
	keys   *riftclient.AccountKeys // nil if no Rift Key: DMs unavailable

	stop context.CancelFunc

	cacheLock sync.RWMutex
	shards    map[string]*riftclient.Shard
	channels  map[string]*riftclient.Channel
	dms       map[string]*riftclient.DMChannel
	splinters map[string]*riftclient.Splinter
	peerKeys  map[string]peerKey // user ID -> signature-checked X25519 identity key
	profiles  *profileCache

	membersLock  sync.Mutex
	shardMembers map[string]*bridgev2.ChatMemberList // shard ID -> members, reset on reconnect
}

var (
	_ bridgev2.NetworkAPI                    = (*RiftClient)(nil)
	_ bridgev2.EditHandlingNetworkAPI        = (*RiftClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*RiftClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI    = (*RiftClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*RiftClient)(nil)
)

func newRiftClient(main *RiftConnector, login *bridgev2.UserLogin) (*RiftClient, error) {
	meta := login.Metadata.(*UserLoginMetadata)
	rc := &RiftClient{
		main:      main,
		login:     login,
		client:    riftclient.NewClient(meta.AccessToken, meta.RefreshToken),
		shards:    map[string]*riftclient.Shard{},
		channels:  map[string]*riftclient.Channel{},
		dms:       map[string]*riftclient.DMChannel{},
		splinters: map[string]*riftclient.Splinter{},
		peerKeys:  map[string]peerKey{},

		shardMembers: map[string]*bridgev2.ChatMemberList{},
	}
	rc.profiles = newProfileCache(rc.client)
	if meta.AccountSeed != "" {
		seed, err := hex.DecodeString(meta.AccountSeed)
		if err != nil {
			return nil, fmt.Errorf("invalid stored account seed: %w", err)
		}
		if rc.keys, err = riftclient.DeriveKeysFromSeed(seed); err != nil {
			return nil, err
		}
	}
	rc.client.OnRequest = func(method, path string, status int, dur time.Duration, err error) {
		login.Log.Debug().Err(err).Str("method", method).Str("path", path).Int("status", status).
			Dur("duration", dur).Msg("Rift API request")
	}
	rc.client.OnTokensRefreshed = func(access, refresh string) {
		meta.AccessToken, meta.RefreshToken = access, refresh
		if err := login.Save(context.Background()); err != nil {
			login.Log.Err(err).Msg("Failed to save refreshed tokens")
		}
	}
	return rc, nil
}

func (rc *RiftClient) Connect(ctx context.Context) {
	ctx, rc.stop = context.WithCancel(rc.login.Log.WithContext(context.Background()))
	go rc.ensureSpaceAvatar(ctx)
	go rc.runLoop(ctx)
}

func (rc *RiftClient) Disconnect() {
	if rc.stop != nil {
		rc.stop()
	}
}

func (rc *RiftClient) runLoop(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		rc.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
		if err := rc.refreshCache(ctx); err != nil {
			if errors.Is(err, riftclient.ErrUnauthorized) {
				rc.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "rift-unauthorized", Message: err.Error()})
				return
			}
			log.Err(err).Msg("Failed to fetch chat list")
		} else {
			rc.syncChats(ctx)
		}
		gw := rc.client.NewGateway(func(evt *riftclient.Event) { rc.handleEvent(ctx, evt) })
		start := time.Now()
		err := gw.Run(ctx, rc.onGatewayReady)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, riftclient.ErrUnauthorized) {
			rc.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateBadCredentials, Error: "rift-unauthorized", Message: err.Error()})
			return
		}
		rc.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Error: "rift-gateway-disconnected", Message: err.Error()})
		log.Warn().Err(err).Dur("retry_in", backoff).Msg("Gateway disconnected")
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (rc *RiftClient) onGatewayReady(ctx context.Context, gw *riftclient.Gateway) {
	rc.cacheLock.RLock()
	shardIDs := make([]string, 0, len(rc.shards))
	for id := range rc.shards {
		shardIDs = append(shardIDs, id)
	}
	rc.cacheLock.RUnlock()
	for _, id := range shardIDs {
		if err := gw.SubscribeShard(ctx, id); err != nil {
			zerolog.Ctx(ctx).Err(err).Str("shard_id", id).Msg("Failed to subscribe to shard")
		}
	}
	rc.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

func (rc *RiftClient) refreshCache(ctx context.Context) error {
	rc.membersLock.Lock()
	clear(rc.shardMembers)
	rc.membersLock.Unlock()
	rc.cacheLock.Lock()
	clear(rc.peerKeys)
	rc.cacheLock.Unlock()
	shards, err := rc.client.ListShards(ctx)
	if err != nil {
		return err
	}
	dms, err := rc.client.ListDMs(ctx)
	if err != nil {
		return err
	}
	// Splinters are optional: a failure here shouldn't take Shards and DMs down with it.
	splinters, err := rc.client.ListSplinters(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to list Splinters")
	}
	channels := map[string]*riftclient.Channel{}
	for _, s := range shards {
		boot, err := rc.client.ShardBootstrap(ctx, s.ID)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Str("shard_id", s.ID).Msg("Failed to bootstrap shard")
			continue
		}
		for _, ch := range boot.Channels {
			channels[ch.ID] = ch
		}
	}
	rc.cacheLock.Lock()
	defer rc.cacheLock.Unlock()
	for _, s := range shards {
		rc.shards[s.ID] = s
	}
	for id, ch := range channels {
		rc.channels[id] = ch
	}
	for _, dm := range dms {
		rc.dms[dm.ID] = dm
	}
	if err == nil {
		clear(rc.splinters) // left or deleted while offline
		for _, s := range splinters {
			rc.splinters[s.ID] = s
		}
	}
	return nil
}

// refreshSplinters re-lists Splinters, for events about one that isn't cached yet (e.g. just added to it).
func (rc *RiftClient) refreshSplinters(ctx context.Context) error {
	splinters, err := rc.client.ListSplinters(ctx)
	if err != nil {
		return err
	}
	rc.cacheLock.Lock()
	defer rc.cacheLock.Unlock()
	clear(rc.splinters)
	for _, s := range splinters {
		rc.splinters[s.ID] = s
	}
	return nil
}

// bridgedChannelTypes are the Shard channel types that carry plain chat messages.
var bridgedChannelTypes = map[string]bool{"text": true, "announcement": true}

// syncChats creates or updates a portal for every Shard space, DM, Splinter and text channel. LatestMessageTS lets
// bridgev2 run a catch-up backfill for chats that got messages while the bridge was offline.
func (rc *RiftClient) syncChats(ctx context.Context) {
	type chat struct {
		key    networkid.PortalKey
		latest time.Time
	}
	rc.cacheLock.RLock()
	chats := make([]chat, 0, len(rc.shards)+len(rc.dms)+len(rc.splinters)+len(rc.channels))
	// Shard spaces first, so their name and icon stay current (they were only set on creation).
	for id := range rc.shards {
		chats = append(chats, chat{key: shardPortalKey(id, rc.login)})
	}
	for id, dm := range rc.dms {
		chats = append(chats, chat{dmPortalKey(id, rc.login), dm.LastActivityAt})
	}
	for id, s := range rc.splinters {
		chats = append(chats, chat{splinterPortalKey(id, rc.login), s.LastActivityAt})
	}
	for id, ch := range rc.channels {
		if bridgedChannelTypes[ch.ChannelType] {
			c := chat{key: channelPortalKey(id, rc.login)}
			if ch.LastMessageAt != nil {
				c.latest = *ch.LastMessageAt
			}
			chats = append(chats, c)
		}
	}
	rc.cacheLock.RUnlock()
	zerolog.Ctx(ctx).Debug().Int("count", len(chats)).Msg("Syncing chats")
	for _, c := range chats {
		rc.login.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta:       simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: c.key, CreatePortal: true},
			GetChatInfoFunc: rc.GetChatInfo,
			LatestMessageTS: c.latest,
		})
	}
}

func (rc *RiftClient) IsLoggedIn() bool {
	access, _ := rc.client.Tokens()
	return access != ""
}

func (rc *RiftClient) LogoutRemote(ctx context.Context) {
	rc.Disconnect()
	if err := rc.client.Logout(ctx); err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to log out of Rift")
	}
}

func (rc *RiftClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	return string(userID) == string(rc.login.ID)
}

var riftFileFeatures = &event.FileFeatures{
	MimeTypes:        map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
	Caption:          event.CapLevelFullySupported,
	MaxCaptionLength: 8000,
	MaxSize:          50 * 1024 * 1024, // free tier; the upload token reports the real limit
}

// riftFormatting lists what content_ast can carry (see format.go).
var riftFormatting = event.FormattingFeatureMap{
	event.FmtBold:                event.CapLevelFullySupported,
	event.FmtItalic:              event.CapLevelFullySupported,
	event.FmtUnderline:           event.CapLevelFullySupported,
	event.FmtStrikethrough:       event.CapLevelFullySupported,
	event.FmtInlineCode:          event.CapLevelFullySupported,
	event.FmtCodeBlock:           event.CapLevelFullySupported,
	event.FmtSyntaxHighlighting:  event.CapLevelFullySupported,
	event.FmtBlockquote:          event.CapLevelFullySupported,
	event.FmtInlineLink:          event.CapLevelFullySupported,
	event.FmtUserLink:            event.CapLevelFullySupported,
	event.FmtUnorderedList:       event.CapLevelFullySupported,
	event.FmtOrderedList:         event.CapLevelFullySupported,
	event.FmtListStart:           event.CapLevelFullySupported,
	event.FmtSpoiler:             event.CapLevelFullySupported,
	event.FmtTextForegroundColor: event.CapLevelFullySupported,
	event.FmtHorizontalLine:      event.CapLevelFullySupported,
	event.FmtHeaders:             event.CapLevelFullySupported,
	event.FmtSuperscript:         event.CapLevelFullySupported,
	event.FmtSubscript:           event.CapLevelFullySupported,
	event.FmtTable:               event.CapLevelFullySupported,
}

var (
	channelCaps = &event.RoomFeatures{
		ID:            "fi.mau.rift.capabilities.2026_11_22.channel",
		Formatting:    riftFormatting,
		MaxTextLength: 8000,
		Edit:          event.CapLevelFullySupported,
		Delete:        event.CapLevelFullySupported,
		Reaction:      event.CapLevelFullySupported,
		Reply:         event.CapLevelFullySupported,
		File: event.FileFeatureMap{
			event.MsgImage:  riftFileFeatures,
			event.MsgVideo:  riftFileFeatures,
			event.MsgAudio:  riftFileFeatures,
			event.MsgFile:   riftFileFeatures,
			event.CapMsgGIF: riftFileFeatures,
		},
	}
	// DM attachments aren't E2EE (only the text is), same as on Rift itself.
	dmCaps = &event.RoomFeatures{
		ID:            "fi.mau.rift.capabilities.2026_11_22.dm",
		Formatting:    riftFormatting,
		File:          channelCaps.File,
		MaxTextLength: 8000,
		Edit:          event.CapLevelFullySupported,
		Delete:        event.CapLevelFullySupported,
		Reaction:      event.CapLevelFullySupported,
		Reply:         event.CapLevelFullySupported,
	}
)

func (rc *RiftClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	if portal.Metadata.(*PortalMetadata).Kind == PortalKindDM {
		return dmCaps
	}
	return channelCaps
}
