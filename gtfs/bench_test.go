package gtfs

import (
	"os"
	"testing"
)

// BenchmarkFuzzBench_QueryArrivals measures the full query path against the
// real fixture StaticDB with live delays applied at fixtureNow. Reports
// allocs so that allocation regressions are visible in benchmark diffs.
func BenchmarkFuzzBench_QueryArrivals(b *testing.B) {
	db, err := BuildFromZIPFile("testdata/gtfs_static.zip", fixtureStops())
	if err != nil {
		b.Fatalf("loading fixture db: %v", err)
	}
	rawFeed, err := os.ReadFile("testdata/tripupdates.gtfsr")
	if err != nil {
		b.Fatalf("reading fixture feed: %v", err)
	}
	poller := NewPoller("", "", NewDB(db))
	withClock(poller.Store(), fixtureNow())
	if err := poller.parse(rawFeed); err != nil {
		b.Fatalf("parse fixture feed: %v", err)
	}
	store := poller.Store()
	now := fixtureNow()
	stops := fixtureStops()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, stop := range stops {
			_ = QueryArrivals(db, store, stop, now, 90, 0, nil)
		}
	}
}

// BenchmarkFuzzBench_PollerParse measures Poller.parse against the full
// captured realtime feed (776 KB, 2857 entities, 15666 stop_time_updates).
func BenchmarkFuzzBench_PollerParse(b *testing.B) {
	db, err := BuildFromZIPFile("testdata/gtfs_static.zip", fixtureStops())
	if err != nil {
		b.Fatalf("loading fixture db: %v", err)
	}
	rawFeed, err := os.ReadFile("testdata/tripupdates.gtfsr")
	if err != nil {
		b.Fatalf("reading fixture feed: %v", err)
	}
	poller := NewPoller("", "", NewDB(db))
	withClock(poller.Store(), fixtureNow())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := poller.parse(rawFeed); err != nil {
			b.Fatalf("parse: %v", err)
		}
	}
}

// BenchmarkFuzzBench_BuildFromZIPFile measures the static GTFS parser from
// the committed trimmed fixture (33 KB, 3 stops, 1285 trips, 9 routes).
func BenchmarkFuzzBench_BuildFromZIPFile(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		db, err := BuildFromZIPFile("testdata/gtfs_static.zip", fixtureStops())
		if err != nil {
			b.Fatalf("build from zip: %v", err)
		}
		_ = db
	}
}

// BenchmarkFuzzBench_GetDelay measures the binary-search delay lookup with a
// 100-entry sorted slice and a mid-range stop sequence query.
func BenchmarkFuzzBench_GetDelay(b *testing.B) {
	ls := NewLiveStore()
	delays := make([]StopDelay, 100)
	for i := range delays {
		delays[i] = StopDelay{
			StopSequence: int32(i * 5),
			DelaySeconds: int32(i * 10),
		}
	}
	ls.Delays["bench_trip"] = delays

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ls.GetDelay("bench_trip", 250) // mid-slice lookup via binary search
	}
}
