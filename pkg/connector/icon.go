package connector

import (
	"context"
	_ "embed"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

//go:embed assets/rift-icon.png
var riftIcon []byte

const kvNetworkIcon database.Key = "rift_network_icon_v1"

// ensureNetworkIcon uploads the Rift logo once and remembers its mxc URI, which GetName reports.
func (rc *RiftConnector) ensureNetworkIcon(ctx context.Context) {
	if mxc := rc.Bridge.DB.KV.Get(ctx, kvNetworkIcon); mxc != "" {
		rc.networkIcon = id.ContentURIString(mxc)
		return
	}
	mxc, _, err := rc.Bridge.Bot.UploadMedia(ctx, "", riftIcon, "rift.png", "image/png")
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to upload network icon")
		return
	}
	rc.networkIcon = mxc
	rc.Bridge.DB.KV.Set(ctx, kvNetworkIcon, string(mxc))
}

// botDisplayName is used when the config leaves appservice.bot.displayname empty (bbctl configs do).
const botDisplayName = "Rift bridge bot"

// updateBotProfile gives the bridge bot the Rift logo and a name. bbctl-generated configs leave both
// empty, and Beeper shows the bot's avatar for the bridge, so without this it gets a generic icon.
func (rc *RiftConnector) updateBotProfile(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	if rc.networkIcon != "" {
		if err := rc.Bridge.Bot.SetAvatarURL(ctx, rc.networkIcon); err != nil {
			log.Warn().Err(err).Msg("Failed to set bot avatar")
		}
	}
	if err := rc.Bridge.Bot.SetDisplayName(ctx, botDisplayName); err != nil {
		log.Warn().Err(err).Msg("Failed to set bot displayname")
	}
}

// ensureSpaceAvatar backfills the Rift logo and m.bridge metadata on the login's personal space
// and management room. bridgev2 only sets these on room creation, so rooms created before the icon
// existed (and SpaceRoom's m.bridge, which ResendBridgeInfo doesn't touch) would otherwise stay blank.
func (rc *RiftClient) ensureSpaceAvatar(ctx context.Context) {
	icon := rc.main.networkIcon
	if icon == "" {
		return
	}
	log := zerolog.Ctx(ctx)
	bot := rc.main.Bridge.Bot
	if rc.login.SpaceRoom != "" {
		if _, err := bot.SendState(ctx, rc.login.SpaceRoom, event.StateRoomAvatar, "", &event.Content{
			Parsed: &event.RoomAvatarEventContent{URL: icon},
		}, time.Now()); err != nil {
			log.Warn().Err(err).Msg("Failed to set personal space avatar")
		}
		if _, err := bot.SendState(ctx, rc.login.SpaceRoom, event.StateBridge, "", &event.Content{
			Parsed: &event.BridgeEventContent{
				BridgeBot: bot.GetMXID(),
				Protocol:  rc.main.GetName().AsBridgeInfoSection(),
				Channel: event.BridgeInfoSection{
					ID:       "__personal_filtering_space__",
					Receiver: string(rc.login.ID),
				},
				BeeperRoomTypeV2: "personal_filtering_space",
			},
		}, time.Now()); err != nil {
			log.Warn().Err(err).Msg("Failed to update personal space bridge info")
		}
	}
	if rc.login.User != nil && rc.login.User.ManagementRoom != "" {
		if _, err := bot.SendState(ctx, rc.login.User.ManagementRoom, event.StateRoomAvatar, "", &event.Content{
			Parsed: &event.RoomAvatarEventContent{URL: icon},
		}, time.Now()); err != nil {
			log.Warn().Err(err).Msg("Failed to set management room avatar")
		}
	}
}
