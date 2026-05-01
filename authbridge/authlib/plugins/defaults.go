package plugins

import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
)

var (
	DefaultInboundPlugins  = []string{"jwt-validation"}
	DefaultOutboundPlugins = []string{"token-exchange"}
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
