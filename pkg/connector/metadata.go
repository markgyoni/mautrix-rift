package connector

import (
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type UserLoginMetadata struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// AccountSeed is the hex Ed25519 account seed derived from the Rift Key. All DM keys derive from it.
	AccountSeed string `json:"account_seed,omitempty"`
}

const (
	PortalKindShard   = "shard"   // Matrix space
	PortalKindChannel = "channel" // Shard text channel
	PortalKindDM      = "dm"
)

type PortalMetadata struct {
	Kind        string `json:"kind"`
	ShardID     string `json:"shard_id,omitempty"`
	RecipientID string `json:"recipient_id,omitempty"`
	// PeerIdentityKey is the DM recipient's X25519 key (hex) pinned on first use. Rift keys never
	// rotate, so a change is surfaced as a warning in the room (like Rift's "safety number changed").
	PeerIdentityKey string `json:"peer_identity_key,omitempty"`
}

// Beeper runs bridges with split_portals, so every portal (including spaces) belongs to one login.
const shardPortalPrefix = "shard:"

func shardPortalKey(shardID string, login *bridgev2.UserLogin) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(shardPortalPrefix + shardID), Receiver: login.ID}
}

func channelPortalKey(channelID string, login *bridgev2.UserLogin) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: login.ID}
}

func dmPortalKey(dmID string, login *bridgev2.UserLogin) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(dmID), Receiver: login.ID}
}
