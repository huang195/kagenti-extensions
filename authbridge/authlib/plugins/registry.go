package plugins

import (
	"fmt"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// PluginFactory creates a pipeline plugin from an auth.Auth instance.
type PluginFactory func(a *auth.Auth) pipeline.Plugin

var registry = map[string]PluginFactory{
	"jwt-validation": func(a *auth.Auth) pipeline.Plugin { return NewJWTValidation(a) },
	"token-exchange": func(a *auth.Auth) pipeline.Plugin { return NewTokenExchange(a) },
}

// Build constructs a pipeline from an ordered list of plugin names.
// Returns an error if any name is not in the registry.
func Build(names []string, a *auth.Auth, opts ...pipeline.Option) (*pipeline.Pipeline, error) {
	ps := make([]pipeline.Plugin, 0, len(names))
	for _, name := range names {
		factory, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown plugin %q", name)
		}
		ps = append(ps, factory(a))
	}
	return pipeline.New(ps, opts...)
}
