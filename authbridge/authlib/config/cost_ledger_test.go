package config

import (
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
