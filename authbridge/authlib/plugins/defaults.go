package plugins

// Default plugin composition (used when the runtime config omits a
// pipeline: section). These are name-only entries; the host is expected
// to supply per-plugin config blocks through the runtime YAML. A mode
// where defaults cannot produce working config without a pipeline:
// section (jwt-validation needs an issuer, token-exchange needs a
// Keycloak URL) means Build will return a validation error — that's
// intentional: silent "works without config" was how the old global-
// block schema hid misconfigurations.
//
// These remain exported so tests can reference the canonical default
// names without repeating the strings.
import (
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
)

var (
	DefaultInboundPlugins  = []config.PluginEntry{{Name: "jwt-validation"}}
	DefaultOutboundPlugins = []config.PluginEntry{{Name: "token-exchange"}}
)
