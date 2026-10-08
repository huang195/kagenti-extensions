"""Env-gated activation hook for the propagate-only OTel shim.

Dockerfile.otel-shim installs this as ``_lineage_propagate.py`` in the app
environment's site-packages next to a one-line ``.pth``, so ``site`` imports
it at every interpreter start, before any app code. The shim thus attaches
through the environment (like ``JAVA_TOOL_OPTIONS`` / ``NODE_OPTIONS``); the
container's command is never rewritten.

Inert unless ``LINEAGE_PROPAGATE=1``. When active: pin the propagate-only
posture as env *defaults* (every exporter ``none``, ``tracecontext,baggage``
propagators — ``setdefault``, so a deliberate override still wins), then run
stock auto-instrumentation via ``initialize()``; which instrumentors activate
depends on what the app imports.

MCP context bridge (mcp 1.x only): the MCP Python SDK's streamable-HTTP
client hands every outbound message over a memory stream to a background task
started when the session was opened, and that task performs the HTTP POST.
Contextvars are copied into a task at creation, so the OTel context there is
the one from session open — none, for a session an app holds across requests —
and the httpx instrumentor would start a fresh trace per tool call. mcp 2.x
carries the sender's context through its streams itself (``_context_streams``);
the 1.x line does not. For a 1.x the hook stamps the caller's OTel context on
each ``SessionMessage`` as it is built (caller's task) and re-attaches it around
the POST (``_handle_post_request``, transport's task); an empty stamp is not
attached. Only the OTel context crosses; nothing else of the caller's task. On
a 2.x the hook does nothing. The stamp is installed wherever ``mcp`` imports,
tool servers included, and is inert there. If the SDK lacks either seam the bridge is
not installed, propagation stays on, and a tool call on a held session roots a
trace of its own (``parent.source=none`` on the tool pod's inbound hop) —
the pre-bridge shape, one warning at startup. A resumed request (the SDK's
``_handle_resumption_request``) is not bridged. Importing the SDK's client
module costs every gated interpreter its import time, once.

Failure policy: never take the app down. ``initialize()`` swallows its own
exceptions, the guard below covers the rest, and ``site`` itself survives a
broken ``.pth`` line. A hook failure therefore means propagation is OFF and the
trace fragments at this pod, visibly (``parent.source=none`` on its outbound
hops) — absent lineage,
never wrong lineage.
"""

import os


def bridge_mcp_context() -> bool:
    """Carry the caller's OTel context across the MCP client transport's task
    boundary, for the mcp 1.x line. Returns True when the bridge is in place
    (or was already); False when ``mcp`` is absent or is a 2.x that propagates
    the context itself (silently), or when a 1.x lacks the seams (one
    warning). A stamp that carries nothing is never attached, so a message
    built with no context leaves a post task's own context alone. Idempotent."""
    import importlib.util

    try:
        from mcp.client.streamable_http import RequestContext, StreamableHTTPTransport
        from mcp.shared.message import SessionMessage
    except ImportError:
        return False
    if importlib.util.find_spec("mcp.shared._context_streams"):
        return False  # mcp >= 2.0 carries the sender's context through its streams itself
    import dataclasses
    import functools
    import inspect
    import logging

    from opentelemetry import context as otel_context

    try:
        original_post = StreamableHTTPTransport._handle_post_request
        if getattr(original_post, "_lineage_bridge", False):
            return True
        if list(inspect.signature(original_post).parameters) != ["self", "ctx"]:
            raise TypeError("_handle_post_request is not (self, ctx)")
        if "session_message" not in {f.name for f in dataclasses.fields(RequestContext)}:
            raise TypeError("RequestContext has no session_message")
        # The stamp needs an instance attribute: refuse a slotted or otherwise
        # closed SessionMessage up front rather than on the app's first message.
        probe = SessionMessage.__new__(SessionMessage)
        probe._lineage_otel_context = None
    except (AttributeError, TypeError, ValueError) as e:
        logging.getLogger(__name__).warning(
            "lineage propagate hook: MCP context bridge not installed (%s); "
            "a tool call on a held MCP session starts a trace of its own",
            e,
        )
        return False

    original_init = SessionMessage.__init__

    @functools.wraps(original_init)
    def init(self, *args, **kwargs):
        original_init(self, *args, **kwargs)
        self._lineage_otel_context = otel_context.get_current()

    @functools.wraps(original_post)
    async def handle_post(self, *args, **kwargs):
        ctx = args[0] if args else kwargs.get("ctx")
        caller = getattr(getattr(ctx, "session_message", None), "_lineage_otel_context", None)
        if not caller:  # no stamp, or an empty context: nothing to carry
            return await original_post(self, *args, **kwargs)
        token = otel_context.attach(caller)
        try:
            return await original_post(self, *args, **kwargs)
        finally:
            otel_context.detach(token)

    handle_post._lineage_bridge = True
    SessionMessage.__init__ = init
    StreamableHTTPTransport._handle_post_request = handle_post
    return True


if os.environ.get("LINEAGE_PROPAGATE") == "1":
    os.environ.setdefault("OTEL_TRACES_EXPORTER", "none")
    os.environ.setdefault("OTEL_METRICS_EXPORTER", "none")
    os.environ.setdefault("OTEL_LOGS_EXPORTER", "none")
    os.environ.setdefault("OTEL_PROPAGATORS", "tracecontext,baggage")
    try:
        from opentelemetry.instrumentation.auto_instrumentation import initialize

        initialize()
    # Deliberately broad: never break the app this hook rides in.
    except Exception:  # noqa: BLE001
        import logging

        logging.getLogger(__name__).exception(
            "lineage propagate hook failed to initialize; propagation is OFF for this process"
        )
    else:
        try:
            bridge_mcp_context()
        except Exception:  # noqa: BLE001
            import logging

            logging.getLogger(__name__).exception(
                "lineage propagate hook: MCP context bridge failed; "
                "a tool call on a held MCP session starts a trace of its own"
            )
