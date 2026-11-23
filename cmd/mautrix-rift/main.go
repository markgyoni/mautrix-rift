package main

import (
	"strings"

	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/markgyoni/mautrix-rift/pkg/connector"
)

// Set by build flags.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// baseVersion is reported for untagged builds; release builds use their git tag (vX.Y.Z).
const baseVersion = "0.1.0"

func main() {
	version := baseVersion
	if v, ok := strings.CutPrefix(Tag, "v"); ok {
		version = v
	}
	m := mxmain.BridgeMain{
		Name:        "mautrix-rift",
		URL:         "https://github.com/markgyoni/mautrix-rift",
		Description: "A Matrix-Rift puppeting bridge.",
		Version:     version,
		Connector:   &connector.RiftConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
