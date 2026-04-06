package main

import (
	"os"

	"code.forgejo.org/forgejo/runner/v12/act/plugin"
	"code.forgejo.org/forgejo/runner/v12/act/plugin/testplugin"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	if os.Getenv(plugin.Handshake.MagicCookieKey) != "" {
		goplugin.Serve(&goplugin.ServeConfig{
			HandshakeConfig: plugin.Handshake,
			Plugins: map[string]goplugin.Plugin{
				plugin.PluginName: &plugin.BackendGRPCPlugin{Impl: testplugin.New()},
			},
			GRPCServer: goplugin.DefaultGRPCServer,
		})
		return
	}

	os.Exit(1)
}
