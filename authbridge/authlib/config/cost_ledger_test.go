package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfig_CostLedgerSectionParses(t *testing.T) {
	var c Config
	src := `
mode: proxy-sidecar
cost_ledger:
  enabled: true
  dir: /var/lib/cortex/cost
  retention_days: 7
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
	if c.CostLedger.RetentionDays != 7 {
		t.Errorf("retention_days = %d, want 7", c.CostLedger.RetentionDays)
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
		{"block present but enabled unset, local", "mode: proxy-sidecar\ncost_ledger:\n  retention_days: 5\n", true, true},
		{"block present but enabled unset, cluster", "mode: proxy-sidecar\ncost_ledger:\n  retention_days: 5\n", false, false},
		{"explicit false beats the local default", "mode: proxy-sidecar\ncost_ledger:\n  enabled: false\n", true, false},
		{"explicit true beats the cluster default", "mode: proxy-sidecar\ncost_ledger:\n  enabled: true\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
				t.Fatalf("yaml: %v", err)
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
func TestCostLedgerConfig_RejectsARetentionShorterThanTheLongestWindow(t *testing.T) {
	for _, days := range []int{1, 2, 6} {
		c := &CostLedgerConfig{RetentionDays: days}
		err := c.Validate()
		if err == nil {
			t.Errorf("retention_days: %d accepted; window=7d would report a partial week as a full one", days)
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
