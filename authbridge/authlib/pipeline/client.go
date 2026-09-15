package pipeline

import "strings"

// EventClient identifies the coding agent that made a request, parsed from its
// User-Agent header.
//
// A DISPLAY AXIS, NOT A SECURITY BOUNDARY. The User-Agent is a request header, so
// this value is CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE: it is an observability
// and cost-attribution key, never an authorization subject. Any client can claim
// to be any agent at any version. Nothing here may ever be used for an
// authorization decision, a rate-limit exemption, a pricing tier, or any other
// choice whose wrong answer costs something. What this is for is attribution in a
// cost breakdown, where a caller lying about itself mis-attributes that caller's
// own spend and nothing else.
//
// That is the same caveat Context.Session carries about client-asserted session
// ids, deliberately in the same words, because it is the same property of a
// different field.
//
// NOT TO BE CONFUSED WITH Identity / EventIdentity, which sit beside it on the
// same context and the same event. Those are the AUTHENTICATED auth principal,
// established by an auth plugin and nil until one runs. This is a self-reported
// software label. The two answer different questions — "who is calling" versus
// "what program is calling" — and the proximity of the fields is the reason this
// paragraph exists: a reader who treats one as the other has built authorization
// on a request header.
//
// Nil means NO User-Agent was sent. That is distinct from a User-Agent that was
// sent but matched no known agent, which produces a non-nil value carrying only
// Raw. The distinction is load-bearing for #952, which has to tell an honest zero
// from missing data: an unrecognised agent we can still NAME is data, one we
// cannot is not.
type EventClient struct {
	// Name is the canonical agent name — "claude-code" — or empty when the
	// User-Agent matched nothing in knownClients. Empty is not a failure; see Raw.
	Name string `json:"name,omitempty"`
	// Version is the version string that followed the product token, or empty when
	// the agent was not recognised or sent no version.
	Version string `json:"version,omitempty"`
	// Raw is the User-Agent verbatim, capped at maxClientLen.
	//
	// Kept ALONGSIDE Name rather than only when parsing fails, so a new coding
	// agent appears in the breakdown the day someone runs it instead of after a
	// parser update ships — and so the detection work for the other agents can see
	// what strings to expect rather than guessing them.
	Raw string `json:"raw,omitempty"`
}

// maxClientLen bounds the retained User-Agent.
//
// Same reasoning as usage.maxLabelLen, and it is the load-bearing bound here
// because this value comes off a request header: its length and its cardinality
// are both chosen off-host. Label() feeds bucket label maps that free a slot only
// a full ring lap later (6h at one minute), so without a cap a caller parks
// arbitrary bytes in this process's memory, from off-host, on the synchronous
// session-append path. The cap is applied BEFORE anything is retained or matched,
// so no field on EventClient — Raw, Version, or the string Label() builds — can
// exceed it by more than the recognised name it is joined to.
//
// 128 is well beyond any real agent's product token and version. Cardinality is
// bounded separately, by usage.maxLabelsPerBucket, which applies to byAgent
// exactly as it does to every other label map.
//
// Truncation is a byte cut, which can split a multi-byte rune and leave invalid
// UTF-8 that a JSON encoder renders as U+FFFD. That is the same behaviour
// usage.truncateLabel already has, and matching it is deliberate: a UA is
// effectively always ASCII, and two different truncation rules for two
// caller-controlled strings is a worse trade than one occasional replacement
// character.
const maxClientLen = 128

// knownClients maps a lowercased User-Agent product token to a canonical agent
// name.
//
// Deliberately small. Claude Code is the only agent with full support today —
// cmd/abctl/toolscan/known.go hardcodes its built-in tool names, so the
// tool-prune analysis only works for it — and OpenCode, Codex and the rest arrive
// with their own detection work rather than a speculative entry here. A guess
// that is wrong is worse than an unrecognised agent, because an unrecognised one
// still shows up under its Raw value and can be identified from the breakdown,
// whereas a mis-mapped one is silently filed under someone else's name.
//
// Keys are the token BEFORE the slash, lowercased. Claude Code sends
// "claude-cli/<version> (external, cli)"; the product token is "claude-cli",
// which is why the key is not the canonical name.
var knownClients = map[string]string{
	"claude-cli": "claude-code",
}

// ParseUserAgent derives an EventClient from a User-Agent header value.
//
// Returns nil when the header is absent or blank — absence, not a client named
// "unknown". Callers must not substitute a placeholder here: EventClient.Label()
// is where absence becomes the display string "unknown", and doing it at parse
// time would make a missing header indistinguishable from an agent that really
// called itself that.
//
// A value that IS present but matches no known agent returns non-nil with an
// empty Name and the (capped) header in Raw. That is an unrecognised agent, not
// an absent one, and Label() reports it under its raw value rather than folding
// it into "unknown" — otherwise every new coding agent would land in the same
// bucket as untagged traffic and the breakdown could not distinguish them.
func ParseUserAgent(ua string) *EventClient {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return nil
	}
	// Capped before ANY of it is retained or matched against, so nothing derived
	// from it downstream can exceed the bound. See maxClientLen.
	if len(ua) > maxClientLen {
		ua = ua[:maxClientLen]
	}
	c := &EventClient{Raw: ua}
	// The product token is the first whitespace-delimited word, so the trailing
	// comment Claude Code appends — "(external, cli)" — is ignored rather than
	// having to be matched. Parsed positionally rather than with a regexp: this
	// runs twice per turn on the request path, and the grammar being read is one
	// slash in one word.
	token := ua
	if i := strings.IndexAny(token, " \t"); i >= 0 {
		token = token[:i]
	}
	product, version, _ := strings.Cut(token, "/")
	if name, ok := knownClients[strings.ToLower(product)]; ok {
		c.Name = name
		c.Version = version
	}
	return c
}

// UnknownClientLabel is the reserved key for traffic that carried no User-Agent.
//
// EXPORTED so there is exactly one definition of it. Both surfaces that serve
// group=agent have to agree on this string: the live aggregator keys its series on
// Label() directly, while the cost ledger stores absence losslessly as "" and maps it
// back at the query boundary (see costledger.labelFor). Those are two different code
// paths reaching the same bucket, so two spellings would surface as two rows in any
// client that merged a ring answer with a ledger answer — and each row would hold half
// the unattributed spend, which is worse than either alone. It was a bare literal in
// both packages, agreeing only by the comment that said it must; this makes the
// agreement something the compiler keeps.
//
// NOT an agent name. See Label for what it means and why a consumer must not present
// it as one.
const UnknownClientLabel = "unknown"

// Label returns the display key for this client: "claude-code/2.1.14" for a
// recognised agent, the raw User-Agent for an unrecognised one, and
// UnknownClientLabel ("unknown") when there is no client at all.
//
// NIL-SAFE ON PURPOSE. Every consumer — the usage aggregator, the cost ledger —
// calls this on events that may carry no client, and a nil check at each call site
// is how one of them eventually gets forgotten. A forgotten one is not a panic
// caught in review either: it is a blank key in a breakdown table, which reads as
// a rendering bug rather than as unattributed traffic.
//
// "unknown" means THIS EVENT CARRIED NO USER-AGENT. It is a RESERVED BUCKET, not
// an agent name, and a consumer must not present it as one: a row labelled
// "unknown" in a cost breakdown is unattributed traffic, not a program that spent
// money. It is never the answer for a User-Agent that WAS sent and could not be
// recognised — that case answers with the raw value, so it stays nameable and does
// not pool with untagged traffic. The empty struct answers "unknown" for the same
// reason: it can name nothing, and inventing a name for it would put a value in a
// cost table that no request ever sent.
//
// A caller that sends literally "User-Agent: unknown" does land in that reserved
// bucket, and that collision is not defended against. Reserving the word would buy
// nothing: the axis is spoofable by construction, so the same caller could just as
// well claim "claude-cli/2.1.14" and land in a real agent's row instead. Anything
// that must not be spoofable belongs on Identity. Recorded here so the collision is
// a known property rather than a surprise to whoever first sees the row.
func (c *EventClient) Label() string {
	if c == nil {
		return UnknownClientLabel
	}
	if c.Name == "" {
		if c.Raw == "" {
			return UnknownClientLabel
		}
		return c.Raw
	}
	if c.Version == "" {
		return c.Name
	}
	return c.Name + "/" + c.Version
}
