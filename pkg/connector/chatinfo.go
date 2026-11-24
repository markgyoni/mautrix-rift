package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

func (rc *RiftClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	id := string(portal.ID)
	if shardID, ok := strings.CutPrefix(id, shardPortalPrefix); ok {
		return rc.shardInfo(shardID)
	}
	for attempt := 0; ; attempt++ {
		rc.cacheLock.RLock()
		ch, isChannel := rc.channels[id]
		dm, isDM := rc.dms[id]
		splinter, isSplinter := rc.splinters[id]
		rc.cacheLock.RUnlock()
		switch {
		case isChannel:
			return rc.channelInfo(ctx, ch), nil
		case isDM:
			return rc.dmInfo(dm), nil
		case isSplinter:
			return rc.splinterInfo(splinter), nil
		}
		// A message in a Splinter we were just added to can arrive before SPLINTER_UPDATE.
		if attempt > 0 || portal.Metadata.(*PortalMetadata).Kind == PortalKindDM {
			return nil, fmt.Errorf("unknown Rift chat %s", id)
		}
		if err := rc.refreshSplinters(ctx); err != nil {
			return nil, fmt.Errorf("unknown Rift chat %s (refreshing Splinters failed: %w)", id, err)
		}
	}
}

func (rc *RiftClient) splinterInfo(s *riftclient.Splinter) *bridgev2.ChatInfo {
	roomType := database.RoomTypeDefault
	members := &bridgev2.ChatMemberList{
		IsFull:  true,
		Members: []bridgev2.ChatMember{{EventSender: rc.selfSender(), Membership: event.MembershipJoin}},
	}
	ownerPL := 50 // shows the Splinter owner as a moderator
	for _, m := range s.Members {
		if m.UserID == string(rc.login.ID) {
			if m.IsOwner {
				members.Members[0].PowerLevel = &ownerPL
			}
			continue
		}
		p := &riftclient.Profile{ID: m.UserID, Username: m.Username, DisplayName: m.DisplayName, AvatarURL: m.AvatarURL}
		rc.profiles.put(p)
		member := bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: networkid.UserID(m.UserID)},
			Membership:  event.MembershipJoin,
			UserInfo:    rc.profileUserInfo(p),
		}
		if m.IsOwner {
			member.PowerLevel = &ownerPL
		}
		members.Members = append(members.Members, member)
	}
	return &bridgev2.ChatInfo{
		Name:         &s.Name,
		Type:         &roomType,
		Avatar:       rc.mediaAvatar(s.IconURL),
		Members:      members,
		CanBackfill:  true,
		ExtraUpdates: setPortalMeta(&PortalMetadata{Kind: PortalKindSplinter}),
	}
}

func (rc *RiftClient) shardInfo(shardID string) (*bridgev2.ChatInfo, error) {
	rc.cacheLock.RLock()
	s, ok := rc.shards[shardID]
	rc.cacheLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown Rift shard %s", shardID)
	}
	roomType := database.RoomTypeSpace
	return &bridgev2.ChatInfo{
		Name:   &s.Name,
		Topic:  s.Description,
		Type:   &roomType,
		Avatar: rc.mediaAvatar(s.IconKey),
		Members: &bridgev2.ChatMemberList{Members: []bridgev2.ChatMember{
			{EventSender: rc.selfSender(), Membership: event.MembershipJoin},
		}},
		ExtraUpdates: setPortalMeta(&PortalMetadata{Kind: PortalKindShard, ShardID: s.ID}),
	}, nil
}

func (rc *RiftClient) channelInfo(ctx context.Context, ch *riftclient.Channel) *bridgev2.ChatInfo {
	parent := shardPortalKey(ch.ShardID, rc.login).ID
	return &bridgev2.ChatInfo{
		Name:         &ch.Name,
		Topic:        ch.Topic,
		ParentID:     &parent,
		Members:      rc.shardMemberList(ctx, ch.ShardID),
		CanBackfill:  true,
		ExtraUpdates: setPortalMeta(&PortalMetadata{Kind: PortalKindChannel, ShardID: ch.ShardID}),
	}
}

// maxShardMembers caps how many members are synced into each channel room.
const maxShardMembers = 2000

// shardMemberList returns the Shard's members (cached per connection). Channel-level permissions
// aren't applied: every channel room gets the whole Shard. On failure only the user is listed.
func (rc *RiftClient) shardMemberList(ctx context.Context, shardID string) *bridgev2.ChatMemberList {
	rc.membersLock.Lock()
	defer rc.membersLock.Unlock()
	if cached, ok := rc.shardMembers[shardID]; ok {
		return cached
	}
	self := bridgev2.ChatMember{EventSender: rc.selfSender(), Membership: event.MembershipJoin}
	members, complete, err := rc.client.ListShardMembers(ctx, shardID, maxShardMembers)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("shard_id", shardID).Msg("Failed to fetch Shard members")
		return &bridgev2.ChatMemberList{Members: []bridgev2.ChatMember{self}}
	}
	list := &bridgev2.ChatMemberList{IsFull: complete, Members: []bridgev2.ChatMember{self}}
	for _, m := range members {
		if m.UserID == string(rc.login.ID) {
			continue
		}
		p := &riftclient.Profile{ID: m.UserID, Username: m.Username, DisplayName: m.DisplayName, AvatarURL: m.AvatarURL}
		rc.profiles.put(p)
		info := rc.profileUserInfo(p)
		list.Members = append(list.Members, bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: networkid.UserID(m.UserID)},
			Membership:  event.MembershipJoin,
			UserInfo:    info,
		})
	}
	rc.shardMembers[shardID] = list
	return list
}

func (rc *RiftClient) dmInfo(dm *riftclient.DMChannel) *bridgev2.ChatInfo {
	roomType := database.RoomTypeDM
	other := networkid.UserID(dm.RecipientID)
	return &bridgev2.ChatInfo{
		Type: &roomType,
		Members: &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: other,
			Members: []bridgev2.ChatMember{
				{EventSender: rc.selfSender(), Membership: event.MembershipJoin},
				{EventSender: bridgev2.EventSender{Sender: other}, Membership: event.MembershipJoin},
			},
		},
		CanBackfill:  true,
		ExtraUpdates: setPortalMeta(&PortalMetadata{Kind: PortalKindDM, RecipientID: dm.RecipientID}),
	}
}

func setPortalMeta(want *PortalMetadata) bridgev2.ExtraUpdater[*bridgev2.Portal] {
	return func(ctx context.Context, portal *bridgev2.Portal) bool {
		meta := portal.Metadata.(*PortalMetadata)
		if meta.Kind == want.Kind && meta.ShardID == want.ShardID && meta.RecipientID == want.RecipientID {
			return false
		}
		// The pinned DM key belongs to the recipient, so it only survives if the recipient does.
		oldRecipient, pinned := meta.RecipientID, meta.PeerIdentityKey
		*meta = *want
		if want.RecipientID != "" && oldRecipient == want.RecipientID {
			meta.PeerIdentityKey = pinned
		}
		return true
	}
}

func (rc *RiftClient) selfSender() bridgev2.EventSender {
	return bridgev2.EventSender{IsFromMe: true, SenderLogin: rc.login.ID, Sender: networkid.UserID(rc.login.ID)}
}

func (rc *RiftClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	p, err := rc.profiles.get(ctx, string(ghost.ID))
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("profile for %s not found", ghost.ID)
	}
	return rc.profileUserInfo(p), nil
}

func (rc *RiftClient) profileUserInfo(p *riftclient.Profile) *bridgev2.UserInfo {
	name := p.Name()
	return &bridgev2.UserInfo{
		Name:        &name,
		Avatar:      rc.mediaAvatar(p.AvatarURL),
		Identifiers: []string{"rift:" + p.Username},
	}
}

// mediaAvatar turns a Rift media key or URL into a bridgev2 avatar. nil key means "no avatar".
func (rc *RiftClient) mediaAvatar(key *string) *bridgev2.Avatar {
	if key == nil || *key == "" {
		return &bridgev2.Avatar{Remove: true}
	}
	k := riftclient.MediaKey(*key)
	// Signed URLs change every time; key the avatar on the stable part.
	stable, _, _ := strings.Cut(k, "?")
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(stable),
		Get: func(ctx context.Context) ([]byte, error) {
			data, _, err := rc.client.DownloadMedia(ctx, k)
			return data, err
		},
	}
}
