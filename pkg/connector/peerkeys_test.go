package connector

import (
	"context"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// Chat resyncs rewrite the portal metadata; the pinned DM key must survive them.
func TestSetPortalMetaKeepsPinnedKey(t *testing.T) {
	portal := &bridgev2.Portal{Portal: &database.Portal{Metadata: &PortalMetadata{
		Kind: PortalKindDM, RecipientID: "alice", PeerIdentityKey: "aa",
	}}}
	meta := portal.Metadata.(*PortalMetadata)
	if setPortalMeta(&PortalMetadata{Kind: PortalKindDM, RecipientID: "alice"})(context.Background(), portal) {
		t.Fatal("unchanged DM reported as changed")
	}
	if meta.PeerIdentityKey != "aa" {
		t.Fatal("pin lost on no-op resync")
	}
	meta.Kind = ""
	setPortalMeta(&PortalMetadata{Kind: PortalKindDM, RecipientID: "alice"})(context.Background(), portal)
	if meta.PeerIdentityKey != "aa" {
		t.Fatal("pin lost when other fields changed")
	}
	setPortalMeta(&PortalMetadata{Kind: PortalKindDM, RecipientID: "bob"})(context.Background(), portal)
	if meta.PeerIdentityKey != "" {
		t.Fatal("pin kept for a different recipient")
	}
}
