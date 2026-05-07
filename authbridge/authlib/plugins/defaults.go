package plugins

import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
)

// DefaultInboundPlugins and DefaultOutboundPlugins are the plugin
// composition used when the runtime config omits a `pipeline:` section.
// Entries carry no config — jwt-validation and token-exchange still read
// their settings from the global auth.Auth at this stage; they'll move
// to per-entry config as they migrate to pipeline.Configurable.
var (
	DefaultInboundPlugins  = []config.PluginEntry{{Name: "jwt-validation"}}
	DefaultOutboundPlugins = []config.PluginEntry{{Name: "token-exchange"}}
)

func DefaultInboundPipeline(a *auth.Auth) (*pipeline.Pipeline, error) {
	return Build(DefaultInboundPlugins, a)
}

func DefaultOutboundPipeline(a *auth.Auth) (*pipeline.Pipeline, error) {
	return Build(DefaultOutboundPlugins, a)
}

// WaypointInboundPipeline creates an inbound pipeline for waypoint mode
// where audience is derived per-request from the destination host.
func WaypointInboundPipeline(a *auth.Auth) (*pipeline.Pipeline, error) {
	jwtPlugin := NewJWTValidation(a, WithAudienceDeriver(routing.ServiceNameFromHost))
	return pipeline.New([]pipeline.Plugin{jwtPlugin})
}
