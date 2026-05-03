package pipeline

import (
	"context"
	"fmt"
	"log/slog"
)

// Pipeline holds an ordered list of plugins and runs them sequentially.
type Pipeline struct {
	plugins []Plugin
}

// defaultSlots lists the built-in extension slot names.
var defaultSlots = map[string]bool{
	"mcp":        true,
	"a2a":        true,
	"security":   true,
	"delegation": true,
	"custom":     true,
}

// Option configures pipeline construction.
type Option func(*options)

type options struct {
	extraSlots []string
}

// WithSlots registers additional valid extension slot names beyond the built-in set.
// Use this when a bridge plugin (e.g., CPEX) produces extensions not in the default set.
func WithSlots(slots ...string) Option {
	return func(o *options) {
		o.extraSlots = append(o.extraSlots, slots...)
	}
}

// New creates a Pipeline from the given plugins after validating capability wiring.
// Returns an error if any plugin declares a read on a slot that no earlier plugin writes.
func New(plugins []Plugin, opts ...Option) (*Pipeline, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	validSlots := make(map[string]bool, len(defaultSlots)+len(o.extraSlots))
	for k, v := range defaultSlots {
		validSlots[k] = v
	}
	for _, s := range o.extraSlots {
		validSlots[s] = true
	}
	if err := validateCapabilities(plugins, validSlots); err != nil {
		return nil, err
	}
	return &Pipeline{plugins: plugins}, nil
}

// Run executes the request phase of the pipeline sequentially.
// If any plugin returns Reject, the pipeline stops and returns that action.
func (p *Pipeline) Run(ctx context.Context, pctx *Context) Action {
	for _, plugin := range p.plugins {
		if ctx.Err() != nil {
			slog.Info("pipeline: request cancelled", "plugin", plugin.Name())
			return Action{Type: Reject, Status: 499, Reason: "request cancelled"}
		}
		action := plugin.OnRequest(ctx, pctx)
		if action.Type == Reject {
			slog.Info("pipeline: plugin rejected request",
				"plugin", plugin.Name(), "status", action.Status, "reason", action.Reason)
			return action
		}
		slog.Debug("pipeline: plugin completed", "plugin", plugin.Name())
	}
	return Action{Type: Continue}
}

// RunResponse executes the response phase in reverse order.
// The last plugin in the chain sees the response first.
func (p *Pipeline) RunResponse(ctx context.Context, pctx *Context) Action {
	for i := len(p.plugins) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			slog.Info("pipeline: response cancelled", "plugin", p.plugins[i].Name())
			return Action{Type: Reject, Status: 499, Reason: "request cancelled"}
		}
		action := p.plugins[i].OnResponse(ctx, pctx)
		if action.Type == Reject {
			slog.Info("pipeline: plugin rejected response",
				"plugin", p.plugins[i].Name(), "status", action.Status, "reason", action.Reason)
			return action
		}
	}
	return Action{Type: Continue}
}

// NeedsBody returns true if any plugin in the pipeline declares BodyAccess.
func (p *Pipeline) NeedsBody() bool {
	for _, plugin := range p.plugins {
		if plugin.Capabilities().BodyAccess {
			return true
		}
	}
	return false
}

// validateCapabilities checks that every slot a plugin reads has been written
// by an earlier plugin in the chain.
func validateCapabilities(plugins []Plugin, validSlots map[string]bool) error {
	written := make(map[string]bool)
	for _, plugin := range plugins {
		caps := plugin.Capabilities()
		for _, slot := range caps.Reads {
			if !validSlots[slot] {
				return fmt.Errorf("plugin %q declares read on unknown slot %q", plugin.Name(), slot)
			}
			if !written[slot] {
				return fmt.Errorf("plugin %q reads slot %q but no earlier plugin writes it", plugin.Name(), slot)
			}
		}
		for _, slot := range caps.Writes {
			if !validSlots[slot] {
				return fmt.Errorf("plugin %q declares write on unknown slot %q", plugin.Name(), slot)
			}
			written[slot] = true
		}
	}
	return nil
}
