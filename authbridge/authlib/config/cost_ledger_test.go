package config

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
	"gopkg.in/yaml.v3"
)

func TestConfig_CostLedgerSectionParses(t *testing.T) {
	var c Config
	// 8, not 7: the floor is usage.Window7dLocalDays, and a fixture below the floor is
	// not a config this loader would accept — see
	// TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches.
	src := `
mode: proxy-sidecar
cost_ledger:
  enabled: true
  dir: /var/lib/cortex/cost
  retention_days: 8
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if c.CostLedger == nil {
		t.Fatal("cost_ledger section did not parse")
	}
	if c.CostLedger.Dir != "/var/lib/cortex/cost" {
		t.Errorf("dir = %q", c.CostLedger.Dir)
	}
	if c.CostLedger.RetentionDays != 8 {
		t.Errorf("retention_days = %d, want 8", c.CostLedger.RetentionDays)
	}
}

// The reason Enabled is a POINTER: an absent block and an explicit `enabled: false`
// must be different answers. With a plain bool the only way to turn the ledger off
// would be to delete the whole block, which also discards the retention setting
// beside it — and the default differs by deployment (on for --local, off in a pod),
// so "unset" cannot be collapsed onto either value.
func TestCostLedgerConfig_UnsetAndExplicitFalseDiffer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		yaml      string
		defaultOn bool
		want      bool
	}{
		{"no block, local default on", "mode: proxy-sidecar\n", true, true},
		{"no block, cluster default off", "mode: proxy-sidecar\n", false, false},
		{"block present but enabled unset, local", "mode: proxy-sidecar\ncost_ledger:\n  retention_days: 8\n", true, true},
		{"block present but enabled unset, cluster", "mode: proxy-sidecar\ncost_ledger:\n  retention_days: 8\n", false, false},
		{"explicit false beats the local default", "mode: proxy-sidecar\ncost_ledger:\n  enabled: false\n", true, false},
		{"explicit true beats the cluster default", "mode: proxy-sidecar\ncost_ledger:\n  enabled: true\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			// This table reaches LedgerEnabled without going through Validate, so an illegal
			// value in a fixture would not fail anything — it would just quietly teach a
			// retention the loader rejects. One of them said retention_days: 5, which stopped
			// being legal when the floor landed beside it, and two said 7, which stopped being
			// legal when the floor was corrected to the eight day files window=7d actually
			// reads. This check is what caught both.
			if c.CostLedger != nil {
				if verr := c.CostLedger.Validate(); verr != nil {
					t.Errorf("fixture is not a config this loader would accept: %v", verr)
				}
			}
			if got := c.CostLedger.LedgerEnabled(tc.defaultOn); got != tc.want {
				t.Errorf("LedgerEnabled(%v) = %v, want %v", tc.defaultOn, got, tc.want)
			}
		})
	}
}

func TestCostLedgerConfig_RejectsNegativeRetention(t *testing.T) {
	c := &CostLedgerConfig{RetentionDays: -1}
	if err := c.Validate(); err == nil {
		t.Error("negative retention_days accepted")
	}
}

// A retention shorter than the longest window the API serves makes that window lie.
// window=7d is answered from these day files, so with retention_days: 2 six of the
// eight local days a 7d window spans have been deleted, and the response still says
// window:"7d", priced:true over two days of spend — indistinguishable downstream from
// a genuinely quiet week.
//
// 7 IS IN THIS TABLE, and it is the case that matters: it is the number an operator
// reading "window=7d" types, it used to be accepted, and it is one day file short —
// the eighth date, which is the oldest and the one the rolling window opens on, had
// already been pruned. See TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches.
func TestCostLedgerConfig_RejectsARetentionShorterThanTheLongestWindow(t *testing.T) {
	for _, days := range []int{1, 2, 6, 7} {
		c := &CostLedgerConfig{RetentionDays: days}
		err := c.Validate()
		if err == nil {
			t.Errorf("retention_days: %d accepted; window=7d reads %d local day files, so this "+
				"would report a partial week as a full one", days, usage.Window7dLocalDays)
			continue
		}
		// The operator who chose the number is the one who reads this, so it has to say
		// why rather than only that.
		if !strings.Contains(err.Error(), "7d") {
			t.Errorf("retention_days: %d rejected with %q, which does not say which window it breaks", days, err)
		}
	}
}

// Zero still means "the package default", so the floor must not turn an absent
// setting into a load failure — that would refuse every config that omits the field.
func TestCostLedgerConfig_ZeroRetentionIsStillTheDefault(t *testing.T) {
	for _, days := range []int{0, minCostLedgerRetentionDays, 30} {
		if err := (&CostLedgerConfig{RetentionDays: days}).Validate(); err != nil {
			t.Errorf("retention_days: %d rejected: %v", days, err)
		}
	}
}

// THE FLOOR USED TO ADMIT THE PARTIAL WEEK IT WAS ADDED TO REJECT. It was the literal
// 7, on the reading that window=7d spans seven days — but usage.ParseWindowSpec makes
// "7d" a ROLLING seven times twenty-four hours, so unless it begins exactly at
// midnight it starts part-way through one date and ends part-way through another and
// the ledger has to open EIGHT day files to answer it. retention_days: 7 therefore
// passed validation and still served window:"7d" over a partial week, which is exactly
// the case the floor exists to prevent.
//
// The day count is derived from ParseWindowSpec here rather than compared against
// usage.Window7dLocalDays, so that this test fails if either the window's definition
// or the floor moves. The second assertion is the one that keeps the constants from
// drifting apart again.
func TestCostLedgerConfig_TheFloorCoversEveryDayTheWindowTouches(t *testing.T) {
	// Mid-afternoon, i.e. not the midnight special case: this is the shape every real
	// request has.
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	spec, err := usage.ParseWindowSpec(usage.Window7d, now)
	if err != nil {
		t.Fatalf("ParseWindowSpec(%q): %v", usage.Window7d, err)
	}
	// The ledger walks whole local days from From to To inclusive (see
	// costledger.Writer.Query), so this counts day files, not hours.
	days := 0
	for d := dayOfTest(spec.From); !d.After(dayOfTest(spec.To)); d = d.AddDate(0, 0, 1) {
		days++
	}

	if minCostLedgerRetentionDays < days {
		t.Errorf("minCostLedgerRetentionDays = %d but window=%q touches %d local day files "+
			"(%s to %s): retention_days: %d passes validation and still answers a rolling week "+
			"over %d of them, which is the partial-week report the floor exists to refuse",
			minCostLedgerRetentionDays, usage.Window7d, days,
			spec.From.Format("2006-01-02"), spec.To.Format("2006-01-02"),
			minCostLedgerRetentionDays, minCostLedgerRetentionDays)
	}
	if minCostLedgerRetentionDays != usage.Window7dLocalDays {
		t.Errorf("minCostLedgerRetentionDays = %d, usage.Window7dLocalDays = %d — the floor must be "+
			"DERIVED from the window it protects, or the two drift again",
			minCostLedgerRetentionDays, usage.Window7dLocalDays)
	}
	// And the boundary is refused, not merely met: one day short of the span is the
	// value an operator actually types.
	if verr := (&CostLedgerConfig{RetentionDays: days - 1}).Validate(); verr == nil {
		t.Errorf("retention_days: %d accepted; it holds one day file fewer than window=%q reads",
			days-1, usage.Window7d)
	}
}

// dayOfTest is local midnight of t. The ledger's own day boundary, spelled here so
// this test does not import costledger — which imports usage, which this package
// already imports, and the point of the test is the arithmetic rather than the wiring.
func dayOfTest(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
