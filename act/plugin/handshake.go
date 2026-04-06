package plugin

import goplugin "github.com/hashicorp/go-plugin"

var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "FORGEJO_RUNNER_PLUGIN",
	MagicCookieValue: "backend",
}

const PluginName = "backend"
