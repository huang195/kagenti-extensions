package costledger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	// EMBEDDED TZDATA, and it is the point of this file rather than a convenience.
	//
	// Every test here needs a zone whose UTC offset CHANGES, because the defect they
	// pin is a local midnight that does not exist — and a time.FixedZone has no
	// transitions, so it cannot express one at all. Real zones come from tzdata, which
	// a scratch container may not carry: time.LoadLocation would then fail, and the
	// obvious response — t.Skip — would turn the whole file into a green pass that
	// asserted nothing, on exactly the platform (CI) where nobody looks. Linking the
	// database into the test binary removes that failure mode: LoadLocation cannot
	// fail for want of files, so mustZone can treat an error as a test failure.
	_ "time/tzdata"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// mustZone loads a real zone, FAILING rather than skipping when it cannot.
//
// t.Fatalf, deliberately. A skip here would report success for a test that never ran
// the code it exists to cover, which is the failure mode that let the local-midnight
// bug ship: the only zone in this package's tests was a fixed offset, so a green suite
// meant nothing about any zone that shifts.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v — this package embeds time/tzdata precisely so this "+
			"cannot happen; a failure here means the import was dropped, not that the host "+
			"lacks a zone database", name, err)
	}
	return loc
}

// dstDay is one zone's DST transition, and what the ledger must do with it.
type dstDay struct {
	zone string
	// date is the LOCAL calendar date of the transition, as a day file is named.
	date string
	// what says how this zone's transition is shaped, so a failure message names the
	// property that broke rather than only the date.
	what string
}

// midnightTransitions are the zones this package got wrong.
//
// THREE REAL SHAPES plus a control, chosen because they fail differently:
//
//   - America/Havana and America/Santiago shift AT 00:00, so local midnight does not
//     exist on the spring day and time.Date normalises it BACKWARDS into the previous
//     day — which named the day file after the wrong date and made a two-day walk open
//     the same file twice.
//   - Asia/Beirut shifts at 00:00 too, but normalises the other way, so the date was
//     right and AddDate stepped PAST the transition day: a walk that should have
//     opened it never did, and that day's spend vanished from the answer.
//   - America/New_York shifts at 02:00, where midnight exists. It is the CONTROL: it
//     passed before this fix and must keep passing, or the fix broke the ordinary case
//     to rescue the unusual one.
//
// Autumn dates are included as well as spring ones. Their midnight exists but occurs
// twice, so they are the case that must NOT regress while the spring case is fixed.
var midnightTransitions = []dstDay{
	{zone: "America/Havana", date: "2026-03-08", what: "spring forward at local midnight"},
	{zone: "America/Havana", date: "2026-11-01", what: "autumn back, midnight occurs twice"},
	{zone: "America/Santiago", date: "2026-09-06", what: "spring forward at local midnight"},
	{zone: "America/Santiago", date: "2026-04-05", what: "autumn back, midnight occurs twice"},
	{zone: "Asia/Beirut", date: "2027-03-29", what: "spring forward at local midnight"},
	{zone: "Asia/Beirut", date: "2026-10-25", what: "autumn back, midnight occurs twice"},
	{zone: "America/New_York", date: "2026-03-08", what: "control: spring forward at 02:00"},
	{zone: "America/New_York", date: "2026-11-01", what: "control: autumn back at 02:00"},
}

// localNoonish is an instant on the given local date, at an hour every zone has.
//
// 09:30 rather than 00:30: the point of these tests is the DAY a row belongs to, and an
// instant inside the transition hour would be testing time.Date's normalisation of the
// input as well as the ledger's handling of it. A workload spending money mid-morning
// is also the ordinary case, which is what makes the resulting loss ordinary too.
func localNoonish(t *testing.T, loc *time.Location, date string) time.Time {
	t.Helper()
	d, err := time.Parse(dayLayout, date)
	if err != nil {
		t.Fatalf("Parse(%q): %v", date, err)
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, loc)
}

// costedEventAt is costedEvent with the instant, host and cost chosen by the caller,
// so a test can put spend on a specific local day in a specific zone.
func costedEventAt(t *testing.T, when time.Time, model string, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: when, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw",
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: 100, OutputTokens: 50,
			TotalTokens: 150, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	}
}

// TestDayOf_NamesTheCalendarDayEvenWhereLocalMidnightDoesNotExist is the unit-level
// statement of the defect: the ledger day of an instant is the date that instant is
// ON, in every zone.
//
// dayOf used to be local midnight, and time.Date normalises a midnight inside a DST
// gap into the neighbouring day — so on a Havana laptop the ledger day of 2026-03-08
// 09:30 was 2026-03-07. Everything else in this file is a consequence of that one
// mapping: the file a row is written to, the files a walk opens, and the date prune
// reads back off a name.
func TestDayOf_NamesTheCalendarDayEvenWhereLocalMidnightDoesNotExist(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			when := localNoonish(t, loc, c.date)
			if got := dayOf(when).Format(dayLayout); got != c.date {
				t.Errorf("dayOf(%s).Format = %s, want %s (%s); the ledger day of an instant "+
					"must be the date that instant is on, or its row is filed under a day no "+
					"query for that day ever visits", when.Format(time.RFC3339), got, c.date, c.what)
			}
			// The zone must actually be the one asked for. A zone that silently resolved to
			// UTC would make every assertion here vacuous.
			if when.Location() != loc {
				t.Fatalf("instant is in %s, want %s", when.Location(), loc)
			}
		})
	}
}

// TestStore_ARowIsWrittenToAndReadFromTheSameDayFileInEveryZone pins the WRITE and
// READ halves against each other, which is the property that decides whether money
// recorded on a day can be reported for that day.
//
// store.path names the file from dayOf and readDay resolves its argument through the
// same function, so the two agreed even while both were wrong — the row was written to
// the previous day's file and read back from it. What did NOT agree is the DATE a
// caller asks about: a query for 2026-03-08 walks to the day whose name is 2026-03-08,
// and on a Havana laptop that file never existed.
func TestStore_ARowIsWrittenToAndReadFromTheSameDayFileInEveryZone(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			when := localNoonish(t, loc, c.date)
			dir := t.TempDir()
			s, err := newStore(dir, 30, loc)
			if err != nil {
				t.Fatalf("newStore: %v", err)
			}
			want := filepath.Join(dir, c.date+".jsonl")
			if got := s.path(when); got != want {
				t.Errorf("path(%s) = %s, want %s (%s)", when.Format(time.RFC3339), got, want, c.what)
			}
			if _, aerr := s.append([]Row{{At: when.Truncate(time.Minute)}}); aerr != nil {
				t.Fatalf("append: %v", aerr)
			}
			if _, serr := os.Stat(want); serr != nil {
				t.Errorf("no day file named for the row's own date: %v; a reader asking for %s "+
					"opens that name and finds nothing", serr, c.date)
			}
			rows, issues, rerr := s.readDay(dayOf(when))
			if rerr != nil {
				t.Fatalf("readDay: %v", rerr)
			}
			if issues != (dayIssues{}) {
				t.Errorf("readDay reported %+v, want a clean read", issues)
			}
			if len(rows) != 1 {
				t.Errorf("readDay returned %d rows, want 1", len(rows))
			}
		})
	}
}

// TestQuery_TheDayWalkOpensEachDayExactlyOnceAcrossADSTMidnight is the money
// assertion: two days of spend must read back as two days of spend.
//
// The walk was `for d := dayOf(from); !d.After(dayOf(to)); d = d.AddDate(0,0,1)` over
// local midnights, and a midnight inside a DST gap breaks it in both directions:
//
//	Havana, 2026-03-07 → 2026-03-08:   opens 2026-03-07 TWICE — every row in it counted
//	                                   twice, so a two-day window reported double
//	Beirut, 2027-03-28 → 2027-03-29:   opens 2026-03-28 ONLY — the transition day's
//	                                   spend is absent from the answer entirely
//
// Both are a wrong dollar figure with nothing in the response saying so, which is the
// one outcome this package treats as worse than an error.
func TestQuery_TheDayWalkOpensEachDayExactlyOnceAcrossADSTMidnight(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			day2 := localNoonish(t, loc, c.date)
			day1 := day2.AddDate(0, 0, -1)
			dir := t.TempDir()
			// The clock's zone is the ledger's day boundary; see New.
			w := newTestWriter(t, dir, func() time.Time { return day2 })
			w.Record("s", costedEventAt(t, day1, "m", 0.25))
			w.Record("s", costedEventAt(t, day2, "m", 0.25))
			if ferr := w.Flush(); ferr != nil {
				t.Fatalf("Flush: %v", ferr)
			}

			rows, err := w.Window(context.Background(), day1.Add(-time.Hour), day2.Add(time.Hour))
			if err != nil {
				t.Fatalf("Window: %v", err)
			}
			var micros int64
			seen := map[string]int{}
			for _, r := range rows {
				micros += r.CostMicros
				seen[r.At.In(loc).Format(dayLayout)]++
			}
			if micros != 500_000 {
				t.Errorf("two days of $0.25 read back as %d micros, want 500000 (%s); rows=%d by day=%v",
					micros, c.what, len(rows), seen)
			}
			for _, d := range []string{day1.Format(dayLayout), c.date} {
				if seen[d] != 1 {
					t.Errorf("day %s appears %d times in the answer, want exactly 1 (%s); by day=%v",
						d, seen[d], c.what, seen)
				}
			}
		})
	}
}

// TestStore_DayFromNameReadsTheDateTheNameSpells is prune's half of the same mapping.
//
// prune parsed a file name with time.ParseInLocation, which resolves "2026-03-08" to
// that day's local midnight — the instant that does not exist in Havana, normalised
// back into 2026-03-07. Every comparison prune makes is against a day derived from
// dayOf, so one skewed date on one side of `Before` decides whether a file lives.
func TestStore_DayFromNameReadsTheDateTheNameSpells(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			s, err := newStore(t.TempDir(), 30, loc)
			if err != nil {
				t.Fatalf("newStore: %v", err)
			}
			day, ok := s.dayFromName(c.date + ".jsonl")
			if !ok {
				t.Fatalf("dayFromName(%q) refused a name this package writes", c.date+".jsonl")
			}
			if got := day.Format(dayLayout); got != c.date {
				t.Errorf("dayFromName(%q) = %s, want %s (%s)", c.date+".jsonl", got, c.date, c.what)
			}
			// And it must be the SAME representation dayOf produces, since prune compares the
			// two directly. Equal instants, not merely equal dates.
			if want := dayOf(localNoonish(t, loc, c.date)); !day.Equal(want) {
				t.Errorf("dayFromName(%q) = %s, want %s — prune compares this against a "+
					"dayOf-derived cutoff, so a different instant for the same date decides "+
					"a deletion by hours", c.date+".jsonl", day.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

// TestPrune_ADayFileNamedOnADSTMidnightIsKeptWhenRetentionCoversIt is the deletion the
// name-parsing skew causes, priced in the only currency that matters here: a day file
// inside the retention window, unlinked.
//
// retainDays 2 keeps today and yesterday. Yesterday is the transition day, whose name
// parsed thirteen hours early — putting it before a cutoff derived from today's ledger
// day, so retention deleted a day of spend it was configured to keep.
func TestPrune_ADayFileNamedOnADSTMidnightIsKeptWhenRetentionCoversIt(t *testing.T) {
	for _, c := range midnightTransitions {
		t.Run(c.zone+"/"+c.date, func(t *testing.T) {
			loc := mustZone(t, c.zone)
			yesterday := localNoonish(t, loc, c.date)
			today := yesterday.AddDate(0, 0, 1)
			// Three days: today, the transition day, and one that retention must still take.
			// The third is what proves the test would notice a prune that deleted nothing.
			old := yesterday.AddDate(0, 0, -2)
			dir := t.TempDir()
			s, err := newStore(dir, 2, loc)
			if err != nil {
				t.Fatalf("newStore: %v", err)
			}
			for _, d := range []time.Time{old, yesterday, today} {
				if _, aerr := s.append([]Row{{At: d.Truncate(time.Minute)}}); aerr != nil {
					t.Fatalf("append: %v", aerr)
				}
			}
			if perr := s.prune(today); perr != nil {
				t.Fatalf("prune: %v", perr)
			}
			exists := func(when time.Time) bool {
				_, serr := os.Stat(s.path(when))
				return serr == nil
			}
			if !exists(today) {
				t.Errorf("today's day file was deleted (%s)", c.what)
			}
			if !exists(yesterday) {
				t.Errorf("the day file for %s was deleted though retainDays=2 covers it (%s); "+
					"its rows are a day of spend and the file is the only copy", c.date, c.what)
			}
			if exists(old) {
				t.Errorf("the day file for %s survived retainDays=2 (%s); retention did not run",
					old.Format(dayLayout), c.what)
			}
		})
	}
}
