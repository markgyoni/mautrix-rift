// Package connector implements the mautrix bridgev2 NetworkConnector for Rift.
package connector

import (
	"context"

	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"
)

type RiftConnector struct {
	Bridge      *bridgev2.Bridge
	networkIcon id.ContentURIString
}

var _ bridgev2.NetworkConnector = (*RiftConnector)(nil)

func (rc *RiftConnector) Init(br *bridgev2.Bridge) {
	rc.Bridge = br
}

func (rc *RiftConnector) Start(ctx context.Context) error {
	rc.ensureNetworkIcon(ctx)
	rc.updateBotProfile(ctx)
	return nil
}

func (rc *RiftConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "Rift",
		NetworkURL:           "https://therift.chat",
		NetworkIcon:          rc.networkIcon,
		NetworkID:            "rift",
		BeeperBridgeType:     "github.com/markgyoni/mautrix-rift",
		DefaultPort:          29340,
		DefaultCommandPrefix: "!rift",
	}
}

func (rc *RiftConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal:    func() any { return &PortalMetadata{} },
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

func (rc *RiftConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}

// GetConfig: no network-specific config yet.
func (rc *RiftConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	return "", nil, configupgrade.NoopUpgrader
}

func (rc *RiftConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 2, 4
}

func (rc *RiftConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	client, err := newRiftClient(rc, login)
	if err != nil {
		return err
	}
	login.Client = client
	return nil
}
