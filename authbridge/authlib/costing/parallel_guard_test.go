package costing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NO TEST IN THIS PACKAGE MAY CALL t.Parallel, AND THAT IS CHECKED RATHER THAN ASKED.
//
// implausible_test.go resets the package-level implausibleWarnOnce and replaces slog.Default()
// so it can observe the single operator warning. Both are process-wide, so a parallel test in
// this package would see another test's logger, or lose its own warning to another test's Once
// — the kind of failure that appears as a flake in whichever test happened to run second.
//
// A comment saying "do not add t.Parallel here" is exactly the instruction that stops being
// true. This is the same instruction, in a form that fails.
func TestNoTestInThisPackageRunsInParallel(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") || name == "parallel_guard_test.go" {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), "t.Parallel(") {
			t.Errorf("%s calls t.Parallel, which is unsafe while this package's tests swap "+
				"implausibleWarnOnce and slog.Default(). Either drop the parallel call or make "+
				"those two injectable first — see the comment on implausibleWarnOnce.", name)
		}
	}
}
