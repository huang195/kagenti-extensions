package pricing

import (
	"fmt"
	"strings"
	"time"

	"github.com/gobwas/glob"
)

// Entry is one row of a rate table: rates for the models matching Model on the
// endpoints matching Host.
//
// Host "" and "*" both mean any endpoint — how the bundled slice is scoped, since
// a shipped table cannot know an operator's gateway names. Model is a glob
// matched case-insensitively.
type Entry struct {
	Host  string
	Model string
	Rates Rates
	Prov  Provenance
	// Currency is the unit Rates are denominated in. EMPTY MEANS CurrencyUSD, matching what an
	// absent pricing.endpoints[].unit means and what every row already on disk means — one
	// default, three places, so they cannot disagree.
	//
	// It rides on the ENTRY rather than on Rates because it is a property of the endpoint the
	// rate came from, not of the numbers: Rates is arithmetic, and a unit is what says which
	// arithmetic is legal.
	Currency string
}

// Table is an immutable resolved rate table. Build one with NewTable; never
// mutate one that is live, because a Registry hands the same pointer to every
// concurrent reader.
type Table struct {
	rows []row
	// mults are endpoint multiplier rules, most specific first. Kept separate from
	// rows because a multiplier is a property of the ENDPOINT, not of a (host, model)
	// pair: one rule scales every model that gateway serves, including ones no row
	// names explicitly.
	mults []multRule

	listFetchedAt time.Time
}

type multRule struct {
	host   string
	spec   specificity
	factor float64
	prov   Provenance
}

type row struct {
	host  string // lower-cased; "" or "*" means any
	model modelMatcher
	spec  specificity
	rates Rates
	prov  Provenance
	// currency is the unit rates are denominated in; never empty once NewTable has run,
	// which is what lets CurrencyFor answer without a second default.
	currency string
}

// modelMatcher is one compiled model pattern.
//
// Compiled with NO separator passed to glob.Compile, so "*" spans both "-" and
// "/" — "*claude-opus-*" has to match "aws/claude-opus-4-1-20250805". That is
// toolprune/pricing.go:65-67's rule, and it differs from the "."-delimited host
// globs elsewhere in core, which is why the two dimensions do not share a
// matcher.
type modelMatcher struct {
	pattern string
	g       *glob.Pattern
	// gPrefixed matches the same pattern behind a provider prefix, so a literal
	// key also matches "anthropic/<key>" and "aws/<key>".
	//
	// This is load-bearing, not a convenience. A metacharacter-free key compiles to
	// a literal matcher, so "claude-opus-4-1" did not match
	// "anthropic/claude-opus-4-1"; that name fell through to the family glob, which
	// carries the NEWEST member's rates. The result was the exact error the
	// generated table exists to fix — opus-4-1 priced at opus-5's $5/Mtok instead
	// of $15 — for every gateway that echoes a provider-prefixed model name, and
	// long-context thresholds were lost the same way.
	gPrefixed *glob.Pattern
	exact     bool // no metacharacters: names exactly one model
}

// globMeta are the characters that make a pattern a glob rather than a literal.
const globMeta = `*?[]{}!\`

func compileModel(pattern string) (modelMatcher, error) {
	lower := strings.ToLower(pattern)
	g, err := glob.Compile(lower)
	if err != nil {
		return modelMatcher{}, fmt.Errorf("pricing: model pattern %q: %w", pattern, err)
	}
	// Only literal keys get the prefix-tolerant form. A pattern that already
	// contains metacharacters is the operator's own business, and silently
	// widening it would make "claude-*" match "vertex_ai/claude-*" against their
	// intent.
	exact := !strings.ContainsAny(lower, globMeta)
	m := modelMatcher{pattern: lower, g: g, exact: exact}
	if exact {
		gp, err := glob.Compile("*/" + lower)
		if err != nil {
			return modelMatcher{}, fmt.Errorf("pricing: model pattern %q: %w", pattern, err)
		}
		m.gPrefixed = gp
	}
	return m, nil
}

// match lower-cases the subject because gateways vary in how they echo model
// names, and a case mismatch would silently unprice the traffic rather than fail
// visibly (toolprune/plugin.go:224-226).
// modelForms holds the progressively-normalized spellings of one wire model name,
// most specific first.
//
// A fixed array rather than a slice so building it costs no allocation: Resolve
// builds one per request and every row matches against the same value.
type modelForms struct {
	v [5]string
	n int
}

func (f *modelForms) add(s string) {
	if f.n > 0 && f.v[f.n-1] == s {
		return // normalization was a no-op at this step
	}
	f.v[f.n] = s
	f.n++
}

// modelNameForms yields the spellings to try, in decreasing specificity.
//
// Progressive rather than all-at-once, and that ordering is the point: normalizing
// fully and then matching once skipped rows that match an INTERMEDIATE form. With
// rows "claude-custom-v2" and "claude-custom", the wire name "claude-custom-v2:0"
// resolved to the latter — ":0" came off to yield an exact row, but stripping
// continued and landed on the less specific one. Trying each form and taking the
// first match cannot do that.
//
// One limit is inherent to the heuristic and worth stating: nothing can distinguish
// a version discriminator from part of a billed name without a table, so
// "claude-custom-v9" resolves to "claude-custom"'s rate rather than being unpriced.
// That is what makes Bedrock's "…-v1:0" work, and it is the trade being made.
func modelNameForms(lower string) modelForms {
	var f modelForms
	f.add(lower)

	s := lower
	// A ":"-delimited tail is a variant selector (":thinking") or Bedrock's version
	// discriminator (":0"), never part of the billed model name.
	if i := strings.LastIndexByte(s, ':'); i > 0 {
		s = s[:i]
		f.add(s)
	}
	// Bedrock appends "-v<n>" after the dated name.
	if i := strings.LastIndex(s, "-v"); i > 0 && isAllDigits(s[i+2:]) {
		s = s[:i]
		f.add(s)
	}
	// A provider prefix, delimited by "/" (aws/, anthropic/) or "." (Bedrock's
	// anthropic.claude-...). Anthropic model names contain no dots, so cutting at
	// the last one is safe for matching.
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
		f.add(s)
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
		f.add(s)
	}
	return f
}

// match reports whether this pattern covers any of the forms, which are ordered
// most-specific first by the caller.
//
// Only LITERAL keys are matched against the normalized forms. A pattern the operator
// wrote with metacharacters is matched against what came off the wire, since
// silently widening it would defeat their intent.
func (m modelMatcher) match(f modelForms) bool {
	if m.g.Match(f.v[0]) {
		return true
	}
	if m.gPrefixed != nil && m.gPrefixed.Match(f.v[0]) {
		return true
	}
	if !m.exact {
		return false
	}
	for i := 1; i < f.n; i++ {
		if m.g.Match(f.v[i]) {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// specificity ranks how tightly a row names its target, for tie-breaks WITHIN one
// provenance level.
//
// The endpoint axis is compared before the model axis. Rates differ more between
// a discounted gateway and vendor list than between two models on one endpoint,
// so a deployment-specific host row must not be shadowed by a broader model glob.
// Within each axis: exact beats glob, then longer beats shorter — toolprune's rule
// (plugin.go:190-209, pricing.go:80-85). The pattern string is the final tie-break
// so two equally specific rows resolve the same way across restarts instead of
// whichever map iteration reached first.
type specificity struct {
	namedHost bool
	// exactHost is true when the host pattern is a literal, so the host it names
	// beats a glob that merely covers it.
	//
	// Its absence was a live defect: at equal pattern length the final tie-break
	// decided, and "*" (0x2A) sorts before any letter, so "*.internal" beat
	// "a.internal" for traffic to a.internal — in either slice order. An operator
	// pinning one discounted gateway beside a broader "*.internal" block silently
	// got the broad rate.
	exactHost bool
	hostLen   int

	exactModel bool
	modelLen   int
	pattern    string
}

func (s specificity) beats(o specificity) bool {
	switch {
	case s.namedHost != o.namedHost:
		return s.namedHost
	case s.exactHost != o.exactHost:
		return s.exactHost
	case s.hostLen != o.hostLen:
		return s.hostLen > o.hostLen
	case s.exactModel != o.exactModel:
		return s.exactModel
	case s.modelLen != o.modelLen:
		return s.modelLen > o.modelLen
	default:
		return s.pattern < o.pattern
	}
}

// outranksOnHost is beats with the model axis removed, for the one ranking that must not read a
// model: an endpoint's unit. It reports false when the two tie on host, and the caller breaks that
// tie on the host pattern — beats would fall through to the model fields instead, so the length of
// a model name in one block could decide which unit another block's host bills in.
func (s specificity) outranksOnHost(o specificity) bool {
	switch {
	case s.namedHost != o.namedHost:
		return s.namedHost
	case s.exactHost != o.exactHost:
		return s.exactHost
	default:
		return s.hostLen > o.hostLen
	}
}

// NewTable compiles entries into a table, rejecting rows that cannot mean
// anything useful.
func NewTable(entries []Entry, mults ...MultiplierRule) (*Table, error) {
	t := &Table{rows: make([]row, 0, len(entries))}
	// Duplicate rows were silently first-wins, so a config listing the same model
	// twice under one endpoint had one of its rates quietly ignored — and which one
	// depended on entry order, which for the config path is YAML map iteration.
	// Rejecting names the collision instead.
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		switch e.Prov {
		case ProvBundled, ProvDiscovered, ProvConfigured:
			// The only levels a table row may carry. An allowlist, not a denylist:
			// Provenance(99) previously passed, outranked every valid row in Resolve
			// because precedence is the numeric ordering, and printed as "none".
		case ProvAuthoritative:
			return nil, fmt.Errorf("pricing: entry %q/%q claims authoritative provenance, which is a settled per-request figure and not a table rate", e.Host, e.Model)
		case ProvNone:
			return nil, fmt.Errorf("pricing: entry %q/%q has no provenance", e.Host, e.Model)
		default:
			return nil, fmt.Errorf("pricing: entry %q/%q has provenance %d, which is not a table level", e.Host, e.Model, int(e.Prov))
		}
		if !e.Rates.any() {
			return nil, fmt.Errorf("pricing: entry %q/%q sets no rate for any tier", e.Host, e.Model)
		}
		m, err := compileModel(e.Model)
		if err != nil {
			return nil, err
		}
		dupKey := strings.ToLower(e.Host) + "\x00" + m.pattern + "\x00" + e.Prov.String()
		if _, dup := seen[dupKey]; dup {
			return nil, fmt.Errorf("pricing: duplicate entry for host %q model %q at %s provenance; one of the two rates would be silently ignored",
				e.Host, e.Model, e.Prov)
		}
		seen[dupKey] = struct{}{}
		host := strings.ToLower(e.Host)
		if !anyHost(host) {
			if err := validHostPattern(host); err != nil {
				return nil, fmt.Errorf("pricing: host pattern %q: %w", e.Host, err)
			}
			// A pattern carrying a port matched NOTHING and was accepted: hostKey
			// strips the port from the endpoint, never from the pattern, so
			// "gw.internal:4000" resolved unpriced for both gw.internal:4000 and
			// gw.internal — or, with the bundled table on, silently billed that
			// gateway at vendor list. Copying the endpoint out of a URL is the most
			// likely way to write this field, so it is rejected with the fix named
			// rather than normalized silently.
			if bare := hostKey(host); bare != host {
				return nil, fmt.Errorf("pricing: host pattern %q must not include a port (ports are stripped from the endpoint before matching, so this would never match); use %q", e.Host, bare)
			}
		}
		// Copy the thresholds: aliasing the caller's slice meant a Table was not
		// actually immutable, so a caller mutating the Entry it passed in would
		// change a live table under concurrent readers.
		rates := e.Rates
		if rates.Thresholds != nil {
			rates.Thresholds = append([]ContextThreshold(nil), rates.Thresholds...)
		}
		t.rows = append(t.rows, row{
			host:  host,
			model: m,
			rates: rates,
			prov:  e.Prov,
			// NORMALISED HERE, once, so no reader downstream needs a second "empty means USD"
			// branch. The config path validates the spelling; a bundled entry names no unit at
			// all, which is exactly the default.
			currency: currencyOrDefault(e.Currency),
			spec: specificity{
				namedHost: !anyHost(host),
				// isIPv6Literal counts as EXACT: matchHost compares such a pattern
				// literally (path.Match would read "[" as a character class), so
				// ranking it as a glob made the matching and the ranking disagree —
				// an overlapping glob of the same length could outrank the very
				// address it was written for.
				exactHost: !anyHost(host) && (isIPv6Literal(host) || !strings.ContainsAny(host, globMeta)),
				// Zero for a catch-all, so "*" and "" rank identically — the docs
				// promise they mean the same thing, but len("*") is 1 and len("") is
				// 0, and hostLen is compared before the model axis, so a
				// {"*", "*"} row used to beat a {"", "claude-opus-5"} row: a
				// catch-all shadowing an exact model.
				hostLen:    hostRankLen(host),
				exactModel: m.exact,
				modelLen:   len(m.pattern),
				pattern:    host + "\x00" + m.pattern,
			},
		})
	}
	// Multiplier rules get the SAME two checks as rate rows above. They were skipped
	// here, and both failures are silent in the same direction: a rule that never
	// matches leaves its gateway at vendor list, which overstates a discounted gateway.
	seenMult := make(map[string]struct{}, len(mults))
	for i, m := range mults {
		where := fmt.Sprintf("pricing multiplier[%d]", i)
		if m.Host != "" {
			where = fmt.Sprintf("pricing multiplier for host %q", m.Host)
		}
		if err := m.validate(where); err != nil {
			return nil, err
		}
		switch m.Prov {
		case ProvBundled, ProvDiscovered, ProvConfigured:
		default:
			return nil, fmt.Errorf("%s: provenance %d is not a table level", where, int(m.Prov))
		}
		host := strings.ToLower(m.Host)
		// A port in the pattern can never match, because hostKey strips the port from
		// the endpoint and never from the pattern. Copying an endpoint out of a URL is
		// the likeliest way to write this field, so name the fix.
		if !anyHost(host) {
			if bare := hostKey(host); bare != host {
				return nil, fmt.Errorf("%s: host pattern must not include a port (ports are stripped from the endpoint before matching, so this would never match); use %q", where, bare)
			}
		}
		// Two rules with the same host and provenance rank equal, so multiplierFor keeps
		// whichever came first and the other factor is silently ignored — and which one
		// wins depends on config iteration order. Rejected, exactly as duplicate rate
		// rows are.
		dupKey := host + "\x00" + m.Prov.String()
		if _, dup := seenMult[dupKey]; dup {
			return nil, fmt.Errorf("%s: duplicate multiplier for this host at %s provenance; one of the two factors would be silently ignored", where, m.Prov)
		}
		seenMult[dupKey] = struct{}{}
		t.mults = append(t.mults, multRule{
			host:   host,
			factor: m.Factor,
			prov:   m.Prov,
			spec: specificity{
				namedHost: !anyHost(host),
				exactHost: !anyHost(host) && (isIPv6Literal(host) || !strings.ContainsAny(host, globMeta)),
				hostLen:   hostRankLen(host),
				pattern:   host,
			},
		})
	}
	return t, nil
}

// multiplierFor returns the most specific matching factor and its provenance.
//
// Ranked by the same host rules as rate rows — a named host beats a catch-all, an exact
// host beats a glob, a longer glob beats a shorter one — with provenance deciding first
// so an operator's explicit factor outranks the shipped one for the same endpoint.
func (t *Table) multiplierFor(endpoint string) (float64, Provenance) {
	var best *multRule
	for i := range t.mults {
		m := &t.mults[i]
		if !matchHost(m.host, endpoint) {
			continue
		}
		if best == nil || m.prov > best.prov || (m.prov == best.prov && m.spec.beats(best.spec)) {
			best = m
		}
	}
	if best == nil {
		return 1, ProvNone
	}
	return best.factor, best.prov
}

// currencyOrDefault reads an empty unit as CurrencyUSD, and any spelling of USD as CurrencyUSD.
//
// ONE PLACE, called where a row is built, so nothing downstream carries its own version of the
// default. Bundled entries name no unit — they are vendor list, in dollars — and a config that
// omits the key means the same thing, so both arrive here empty and leave as USD.
//
// CASE-FOLDED FOR THE DEFAULT ONLY, so every consumer's `== CurrencyUSD` test is right without
// each of them remembering to fold. normaliseUnit already canonicalises what an operator types,
// which covers the config path at its entrance; this covers the rest. Endpoint is an EXPORTED
// struct and core is consumed outside this repo, so a caller can build one with Currency "usd"
// having never gone through a YAML file — and every site that compares exactly would read that
// spelling as a non-default unit. Downstream of this funnel the only
// non-canonical spellings left are units this package has no canonical form for, where the
// operator's own spelling is the name and is preserved on purpose.
func currencyOrDefault(c string) string {
	if c == "" || strings.EqualFold(c, CurrencyUSD) {
		return CurrencyUSD
	}
	return c
}

// CurrencyFor is the unit a figure for this (endpoint, model) pair is denominated in: the
// endpoint's, whatever the model.
//
// THE MODEL DOES NOT CHANGE IT, because a figure can reach the ledger without any row having
// priced it — a cost the gateway reported, which settle publishes as authoritative without
// consulting this table — and that figure is in the gateway's unit. Answering from the row that
// matched the model labelled such a charge with whichever row happened to match, a catch-all's or
// a bundled one's, which is USD. bestRow admits only rows in this same unit, so a figure the table
// did price gets the same answer.
//
// USD FOR A NIL TABLE, rather than empty. The caller is about to label a figure, and empty
// already means USD everywhere downstream — so "" would be the same answer written less legibly. Nil is the Kubernetes deployment, where pricing is not wired: it reports
// the default rather than panicking on the response path, for the reason Resolve answers ProvNone
// there.
//
// A MULTIPLIER CANNOT CHANGE IT. multiplierFor scales a rate WITHIN an endpoint and never crosses
// units — scaling credits by 0.76 leaves credits.
func (t *Table) CurrencyFor(endpoint, model string) string {
	if t == nil {
		return CurrencyUSD
	}
	return t.unitFor(endpoint)
}

// unitFor is the unit an endpoint bills in: that of the best row whose host covers it — bestRow's
// ranking with the model axis removed — or USD when none does.
//
// Provenance first, so a configured row for this gateway decides over a bundled one; bundled rows
// are all USD, so an endpoint no configured block covers is USD, which is every deployment that has
// configured no unit. Config.entries refuses two blocks that name the same host in different units.
//
// So the most specific block covering a host decides its unit, and a block that omits `unit:`
// decides USD for its hosts. Two different patterns of equal host rank — *.bob.ibm.com and
// api.*.ibm.com — are decided by the pattern that sorts first, never by a model name.
func (t *Table) unitFor(endpoint string) string {
	if best := t.bestRowForHost(endpoint); best != nil {
		return best.currency
	}
	return CurrencyUSD
}

// bestRowForHost is bestRow with the model test dropped: the most specific row whose HOST covers
// this endpoint, or nil.
//
// Only unitFor uses it. It must never be used to pick RATES: a row reached without matching the
// model is the wrong row to price from, which is what bestRow's own comment is about.
//
// RANKED ON THE HOST ALONE, tie-broken on the host pattern. Rows Build produces that share one
// pattern at one provenance agree on their unit — Config.entries refuses them otherwise, and
// bundled rows are all USD — so which of them is kept cannot change the answer. A table built
// directly with NewTable is not held to that, so the last tie breaks on the unit itself, keeping
// the answer independent of row order as specificity's final tie-break does.
func (t *Table) bestRowForHost(endpoint string) *row {
	var best *row
	for i := range t.rows {
		r := &t.rows[i]
		if !matchHost(r.host, endpoint) {
			continue
		}
		if best == nil || r.prov > best.prov || (r.prov == best.prov && hostRowBeats(r, best)) {
			best = r
		}
	}
	return best
}

// hostRowBeats reports whether r outranks best for the endpoint's unit at one provenance.
func hostRowBeats(r, best *row) bool {
	if r.spec.outranksOnHost(best.spec) {
		return true
	}
	if best.spec.outranksOnHost(r.spec) {
		return false
	}
	if r.host != best.host {
		return r.host < best.host
	}
	return r.currency < best.currency
}

// Resolve returns the rates for one (endpoint, model) pair and where they came
// from, already flattened for a prompt of promptTotal tokens.
//
// Provenance decides first, specificity only within a level — see the
// specificity type for why the endpoint axis outranks the model axis. A nil Table
// resolves to ProvNone rather than panicking: a binary built without pricing
// wiring must report traffic as unpriced, not crash on the response path.
func (t *Table) Resolve(endpoint, model string, promptTotal int) (Rates, Provenance) {
	if t == nil {
		return Rates{}, ProvNone
	}
	best := t.bestRow(endpoint, model)
	if best == nil {
		return Rates{}, ProvNone
	}
	rates, prov := best.rates.At(promptTotal), best.prov
	// A scaled figure reports the STRONGER of its two sources: bundled x bundled stays
	// bundled, but either half configured makes the result configured.
	//
	// Because "bundled" is the label that means "you have told us nothing about this
	// endpoint, expect it to be wrong" — it is what drives WarnIfUnpinned and what
	// agentop annotates. An operator who set a multiplier HAS told us about their
	// gateway, so reporting bundled would send them to pin rates they have effectively
	// already pinned. The weaker reading looks more conservative and is in fact less
	// informative.
	f, mprov := t.multiplierFor(endpoint)
	// A multiplier WEAKER than the rates it would scale is dropped.
	//
	// The case is an operator who measured their gateway and pinned the real per-model
	// rates for a host that also carries a shipped multiplier: the pinned figures are
	// already post-discount, so scaling them again understates spend by the factor —
	// ~24% for the shipped 0.76, and understating is the direction this package treats
	// as dangerous, because it hides cost rather than exaggerating it.
	//
	// Framed as provenance rather than as a special case for bundled: a scalar derived
	// from list may only scale rates that are themselves list-derived. Configured rates
	// with a configured multiplier still both apply — the operator asked for both, and
	// `agentop pricing --host` shows the factor being applied.
	if mprov < prov {
		f, mprov = 1, ProvNone
	}
	// Provenance is bumped OUTSIDE the f != 1 guard. An operator who writes
	// `multiplier: 1.0` to say "this endpoint bills at list" has told us about their
	// gateway just as much as one who writes 0.76 — that is the whole rationale above —
	// and gating the bump on the factor being interesting would report their deliberate
	// pin as bundled, then send them a WarnIfUnpinned line telling them to pin it.
	// multiplierFor returns ProvNone when nothing matches, so this is a no-op then.
	if mprov > prov {
		prov = mprov
	}
	if f != 1 {
		rates = rates.scale(f)
	}
	return rates, prov
}

// bestRow picks the row that wins for one (endpoint, model) pair, or nil.
//
// Shared with the describe path rather than reimplemented there: two copies of this
// ranking would diverge the first time it is touched, and the divergence would be silent
// — a marker or an annotation attached to a different row than the one being charged.
//
// ONLY ROWS IN THE ENDPOINT'S UNIT are candidates — see unitFor. A dollar row pricing traffic to a
// gateway that bills in credits produces a figure that is neither.
func (t *Table) bestRow(endpoint, model string) *row {
	if t == nil {
		return nil
	}
	unit := t.unitFor(endpoint)
	// Built once per call, not per row: every row matches against the same forms.
	forms := modelNameForms(strings.ToLower(strings.TrimSpace(model)))
	var best *row
	for i := range t.rows {
		r := &t.rows[i]
		if !matchHost(r.host, endpoint) || !r.model.match(forms) || !strings.EqualFold(r.currency, unit) {
			continue
		}
		if best == nil || r.prov > best.prov || (r.prov == best.prov && r.spec.beats(best.spec)) {
			best = r
		}
	}
	return best
}

var _ Resolver = (*Table)(nil)
