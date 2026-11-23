# mautrix-rift

A Matrix ↔ [Rift](https://therift.chat) puppeting bridge, built on
[mautrix-go](https://github.com/mautrix/go)'s bridgev2. It runs as a self-hosted
[Beeper](https://www.beeper.com) bridge via [bbctl](https://github.com/beeper/bridge-manager).

## Features

Shard channels and 1:1 DMs (including Rift's end-to-end encryption): messages, replies, edits,
deletes, reactions, media, formatting, mentions, history backfill, typing and read markers.

Not yet: Splinters (group chats), threads and forums.

## Setup

1. Create a config with [bbctl](https://github.com/beeper/bridge-manager). It registers the bridge
   with your Beeper account:

   ```sh
   bbctl login
   bbctl config --type bridgev2 -o data/config.yaml sh-rift
   ```

2. The container runs as user ID 1337, so give it the `data` folder:

   ```sh
   sudo chown -R 1337:1337 data
   ```

3. Start it with Docker Compose. The config and database live in `data/`:

   ```yaml
   services:
     mautrix-rift:
       image: ghcr.io/markgyoni/mautrix-rift:latest
       restart: unless-stopped
       volumes:
         - ./data:/data
   ```

4. In Beeper, message `@sh-riftbot:beeper.local` with `login`. Enter your Rift username and
   password, then approve the 9-digit code from a device where you're signed in to Rift. You can
   use your 12-word Rift Key instead. Two-factor auth works with both.

Run only one copy of the bridge per config, or the copies will log each other out.

## License

[AGPL-3.0](LICENSE)
