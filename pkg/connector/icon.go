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

// ensureSpaceAvatar sets the Rift logo on the login's personal space. bridgev2 only sets it when it
// creates the space, so spaces created before the icon existed (or by older versions) stay blank.
func (rc *RiftClient) ensureSpaceAvatar(ctx context.Context) {
	icon := rc.main.networkIcon
	if icon == "" || rc.login.SpaceRoom == "" {
		return
	}
	_, err := rc.main.Bridge.Bot.SendState(ctx, rc.login.SpaceRoom, event.StateRoomAvatar, "", &event.Content{
		Parsed: &event.RoomAvatarEventContent{URL: icon},
	}, time.Now())
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to set personal space avatar")
	}
}
