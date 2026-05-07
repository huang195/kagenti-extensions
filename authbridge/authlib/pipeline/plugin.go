package pipeline

import "context"

// Plugin is the interface that all pipeline extensions implement.
type Plugin interface {
	Name() string
	Capabilities() PluginCapabilities
	OnRequest(ctx context.Context, pctx *Context) Action
	OnResponse(ctx context.Context, pctx *Context) Action
}

// PluginCapabilities declares what extension slots a plugin reads and writes.
// The pipeline validates at startup that all reads are satisfied by an earlier
// plugin's writes.
type PluginCapabilities struct {
	Reads      []string // extension slot names this plugin reads
	Writes     []string // extension slot names this plugin writes
	BodyAccess bool     // whether this plugin needs request/response body buffered
}

// Initializer is an optional interface a plugin may implement when it
// needs to run work once before the pipeline starts serving traffic.
// Typical uses: load a model, warm a cache, open a database connection,
// register Prometheus metrics, spawn a background goroutine. Init is
// called by Pipeline.Start exactly once, in plugin declaration order.
// If any plugin's Init returns an error the pipeline fails fast —
// Pipeline.Start returns the error without calling Init on later
// plugins (nothing to unwind: earlier plugins succeeded).
//
// Plugins that don't need initialization simply don't implement this
// interface; the pipeline skips them. Keeping it optional preserves
// backward compatibility with every existing plugin.
type Initializer interface {
	Init(ctx context.Context) error
}

// Shutdowner is an optional interface a plugin may implement when it
// needs to release resources on graceful shutdown. Typical uses: flush
// in-flight audit events, close a DB connection, cancel a background
// goroutine it spawned in Init. Shutdown is called by Pipeline.Stop
// exactly once, in reverse declaration order (LIFO — symmetric with
// OnResponse dispatch) so a plugin that depends on an earlier plugin's
// resources can still use them while shutting down.
//
// Shutdown is best-effort: errors are logged but do not prevent other
// plugins from shutting down. The caller-supplied ctx carries a
// shutdown deadline; plugins must respect it and return rather than
// block indefinitely.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}
