package httpx

import (
	"context"
	"time"
)

// TeardownTimeout bounds work that outlives the request it belongs to.
//
// Ten seconds because the work is finalization — settle a figure, append a session event,
// dispatch finishers — not I/O to an upstream. Long enough that a slow ledger append or a
// plugin's own timeout completes, short enough that a wedged finisher cannot pin a goroutine
// and the buffers it holds for the life of the process.
const TeardownTimeout = 10 * time.Second

// TeardownContext detaches ctx from its parent's cancellation and gives it a deadline.
//
// DETACHED, BECAUSE THE PARENT IS ALREADY DONE. Finalization runs exactly when a request has
// ended — a client hangup, a stream torn down, a response fully written — so the request's own
// context is cancelled and pipeline.RunResponseFrame refuses a cancelled context before calling
// any plugin. Passing it straight through means the work silently does not happen, which is the
// defect this exists to prevent.
//
// AND BOUNDED, WHICH context.WithoutCancel ALONE IS NOT. Detaching removes the only thing that
// would ever stop this work: with no deadline, a plugin that blocks holds a goroutine, its
// buffers and its pctx forever, and nothing in the process notices. The caller must call the
// returned cancel — deferring it is what releases the timer.
func TeardownContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), TeardownTimeout)
}
