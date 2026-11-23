#!/bin/sh
# Requires cgo (mattn/go-sqlite3). goolm = pure-Go Olm, so libolm isn't needed.
set -e
MAUTRIX_VERSION=$(grep 'maunium.net/go/mautrix ' go.mod | awk '{ print $2 }')
# TAG and COMMIT can be passed in (the Docker build has no .git).
TAG=${TAG:-$(git describe --exact-match --tags 2>/dev/null || echo unknown)}
COMMIT=${COMMIT:-$(git rev-parse HEAD 2>/dev/null || echo unknown)}
GO_LDFLAGS="-s -w -X main.Tag=$TAG -X main.Commit=$COMMIT -X 'main.BuildTime=$(date -Iseconds)' -X 'maunium.net/go/mautrix.GoModVersion=$MAUTRIX_VERSION'"
OUT=mautrix-rift
[ "$(go env GOOS)" = windows ] && OUT=mautrix-rift.exe
CGO_ENABLED=1 go build -tags goolm -ldflags="$GO_LDFLAGS" -o "$OUT" "$@" ./cmd/mautrix-rift
