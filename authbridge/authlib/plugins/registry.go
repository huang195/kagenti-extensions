package plugins

import (
	"fmt"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// PluginFactory creates a pipeline plugin from an auth.Auth instance.
//
// auth.Auth is currently passed at construction so jwt-validation and
// token-exchange can use it as their composition point for verifier /
// exchanger / router / bypass. As each plugin migrates to
// pipeline.Configurable (see authlib/plugins/CONVENTIONS.md), it will
// stop reading from auth.Auth and build its internal state from its own
// local config instead.
type PluginFactory func(a *auth.Auth) pipeline.Plugin

var registry = map[string]PluginFactory{
	"jwt-validation":   func(a *auth.Auth) pipeline.Plugin { return NewJWTValidation(a) },
	"token-exchange":   func(a *auth.Auth) pipeline.Plugin { return NewTokenExchange(a) },
	"mcp-parser":       func(_ *auth.Auth) pipeline.Plugin { return NewMCPParser() },
	"a2a-parser":       func(_ *auth.Auth) pipeline.Plugin { return NewA2AParser() },
	"inference-parser": func(_ *auth.Auth) pipeline.Plugin { return NewInferenceParser() },
}

// Build constructs a pipeline from an ordered list of plugin entries.
// For every plugin that implements pipeline.Configurable, Build calls
// Configure with the entry's Config bytes (nil when omitted). Passing
// config to a plugin that doesn't implement Configurable is rejected so
// stale or misplaced config blocks fail at startup instead of being
// silently ignored.
//
// Unknown plugin names fail fast.
func Build(entries []config.PluginEntry, a *auth.Auth, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(entries))
	for _, e := range entries {
		factory, ok := registry[e.Name]
		if !ok {
			return nil, fmt.Errorf("unknown plugin %q", e.Name)
		}
		p := factory(a)
		if c, ok := p.(pipeline.Configurable); ok {
			if err := c.Configure(e.Config); err != nil {
				return nil, fmt.Errorf("configure %q: %w", e.Name, err)
			}
		} else if len(e.Config) > 0 {
			return nil, fmt.Errorf("plugin %q does not accept configuration", e.Name)
		}
		ps = append(ps, p)
	}
	return pipeline.New(ps, opts...)
}
