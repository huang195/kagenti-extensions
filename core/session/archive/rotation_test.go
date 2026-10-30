package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestWrite_RotatesASegmentAtTheByteCap pins issue #1566 item 3. A segment used to close
// only on 60-minute idle, the open-writer cap, or delete/clear/rename/write-failure —
// never on size — and planPrune skips open segments in both its age and maxBytes loops.
// A session busy all day therefore kept one segment growing without limit, and the
// archive's maxBytes was unenforceable against exactly the sessions that produced the
// most bytes. Rotation closes the segment at the cap; the next write opens a fresh one,
// which retention can then actually delete.
func TestWrite_RotatesASegmentAtTheByteCap(t *testing.T) {
	root := t.TempDir()
	clk := newClock()
	// A one-byte cap makes every event its own rotation: the test wants the mechanism,
	// not a particular segment size.
	a := openTest(t, root, clk, WithMaxSegmentBytes(1))
	rec := newRecorder(a)

	var evs []pipeline.SessionEvent
	for i := 0; i < 8; i++ {
		evs = append(evs, pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com",
			Inference: &pipeline.InferenceExtension{Model: "m",
				Messages: []pipeline.InferenceMessage{{Role: "user", Content: "hello"}}},
		})
	}
	rec.record("s1", evs[:5]...)
	// Rotation runs in the tick after the cap is crossed, not in write: a segment's
	// byte count is only real once its zstd encoder has been flushed. The first tick
	// closes the over-cap segment; the writes after it open the next one, which is
	// what puts a second segment on disk.
	settle(a)
	tick(a)
	rec.record("s1", evs[5:]...)
	settle(a)
	// The clock has to move for session.json to be rewritten: writeMeta runs on the
	// metaEvery cadence while dirty, and a frozen clock never crosses it.
	clk.advance(metaEvery + time.Second)
	tick(a)

	dir := filepath.Join(root, dataDirName, dirName("s1"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var segs int
	for _, e := range entries {
		// HasSuffix, not filepath.Ext: the extension is ".jsonl.zst", and Ext
		// stops at the last dot, returning ".zst".
		if strings.HasSuffix(e.Name(), segmentExt) {
			segs++
		}
	}
	if segs < 2 {
		t.Fatalf("session holds %d segment(s), want >= 2: a session written past the byte "+
			"cap must rotate, or its segment stays open forever, planPrune can never delete "+
			"it, and maxBytes is a bound the busiest session is exempt from", segs)
	}

	// Rotation must not lose or reorder anything: every event stays readable through the
	// same reader the request path uses, in sequence order.
	got := onDisk(t, root, "s1")
	if len(got) != len(evs) {
		t.Fatalf("onDisk holds %d events, want %d — rotation must be a file-boundary change, "+
			"not a data change", len(got), len(evs))
	}
	for i := range got {
		if got[i].Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d, want %d: the reader must see the same sequence "+
				"across the rotated boundary", i, got[i].Seq, i+1)
		}
	}
}
