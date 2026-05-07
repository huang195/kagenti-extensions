package plugins

import (
	"fmt"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// PluginFactory returns a fresh plugin instance. Plugins take no
// construction arguments — they receive their configuration through
// pipeline.Configurable.Configure during Build, and any external
// dependencies (JWKS cache, HTTP client, etc.) are built from that
// local config inside Configure.
type PluginFactory func() pipeline.Plugin

var registry = map[string]PluginFactory{
	"jwt-validation":   func() pipeline.Plugin { return NewJWTValidation() },
	"token-exchange":   func() pipeline.Plugin { return NewTokenExchange() },
	"mcp-parser":       func() pipeline.Plugin { return NewMCPParser() },
	"a2a-parser":       func() pipeline.Plugin { return NewA2AParser() },
	"inference-parser": func() pipeline.Plugin { return NewInferenceParser() },
}

// Build constructs a pipeline from an ordered list of plugin entries.
// For every plugin that implements pipeline.Configurable, Build calls
// Configure with the entry's Config bytes (nil when omitted). Passing
// config to a plugin that doesn't implement Configurable is rejected so
// stale or misplaced config blocks fail at startup instead of being
// silently ignored.
//
// Unknown plugin names fail fast.
func Build(entries []config.PluginEntry, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(entries))
	for _, e := range entries {
		factory, ok := registry[e.Name]
		if !ok {
			return nil, fmt.Errorf("unknown plugin %q", e.Name)
		}
		p := factory()
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
