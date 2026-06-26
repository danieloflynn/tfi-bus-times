package gtfs

import (
	"sync"
	"testing"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// This file implements FR5 of the test-suite-hardening PRD: it exercises the
// shared concurrency surface of package gtfs under the race detector. The point
// is NOT to assert a particular arrival list (the golden test does that) but to
// run, simultaneously and for a fixed bounded number of iterations:
//
//   - one writer goroutine repeatedly calling Poller.parse, which swaps the
//     LiveStore's Delays/Cancellations/Additions maps under its write lock and
//     resets the diagLogged dedupe map;
//   - several reader goroutines calling QueryArrivals, which read the LiveStore
//     (GetDelay/IsCancelled/GetAdditions under the RLock) and take the
//     DiagLogOnce *write* lock when a rescue/addition diagnostic fires;
//   - one goroutine swapping the StaticDB snapshot via DB.Store/DB.Load (the
//     atomic.Pointer the poller and readers both Load each pass).
//
// The clock is pinned with withClock BEFORE any goroutine starts, because the
// LiveStore.now field is written once and then only read concurrently — pinning
// it afterwards (or letting parse/Poll reassign it) would itself be a data race.
//
// Verify with:  go test -race ./gtfs/ -run TestConcurrency
//
// A passing run (no "DATA RACE" report, no panic, completion within the bounded
// iteration counts) is the assertion. Helpers are prefixed "concurrency" so they
// don't collide with the other hardening files in this package.

const (
	// Fixed, bounded iteration counts — no time-based loop, so the test is
	// deterministic and fast. Tuned to overlap the writer, readers and the
	// snapshot swapper enough times that -race reliably observes any unguarded
	// access, while keeping the whole test well under a second even with -race.
	concurrencyParseIters      = 25
	concurrencyQueryIters      = 120
	concurrencyQueryGoroutines = 4
	concurrencySwapIters       = 200
)

// TestConcurrency_FixtureFeedRace runs the real captured realtime feed through
// Poller.parse while readers hammer QueryArrivals for the configured stops and a
// third goroutine hot-swaps the StaticDB snapshot — all concurrently, all
// against the same LiveStore and DB holder. It is the canonical FR5 race test.
func TestConcurrency_FixtureFeedRace(t *testing.T) {
	// Two independent, self-consistent snapshots to swap between. Built once up
	// front (parsing the 33 KB trimmed zip is the only non-trivial cost); the
	// swap loop only stores/loads the already-built pointers.
	dbA := loadFixtureDB(t)
	dbB := loadFixtureDB(t)

	holder := NewDB(dbA)
	poller := NewPoller("", "test", holder)

	// Pin the clock BEFORE launching goroutines (see file comment). After this,
	// store.now is only ever read.
	store := withClock(poller.Store(), fixtureNow())
	now := fixtureNow()

	feed := loadFixtureFeed(t)

	// Sanity: one parse must succeed before we fan out, proving the fixture and
	// wiring are valid (a parse error inside a goroutine would otherwise be easy
	// to miss).
	if err := poller.parse(feed); err != nil {
		t.Fatalf("initial parse of fixture feed: %v", err)
	}

	stops := fixtureStops()

	var wg sync.WaitGroup

	// Writer: repeatedly parse the same raw feed. Each call rebuilds and swaps
	// the LiveStore maps under the write lock and resets diagLogged.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < concurrencyParseIters; i++ {
			if err := poller.parse(feed); err != nil {
				t.Errorf("concurrent parse iteration %d: %v", i, err)
				return
			}
		}
	}()

	// Readers: several goroutines querying every configured stop. QueryArrivals
	// Loads the current snapshot, reads the LiveStore under the RLock, and can
	// take the DiagLogOnce write lock — exercising the reader side of every
	// shared structure.
	for g := 0; g < concurrencyQueryGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < concurrencyQueryIters; i++ {
				db := holder.Load()
				for _, stop := range stops {
					_ = QueryArrivals(db, store, stop, now, 120, 0, nil)
				}
			}
		}()
	}

	// Swapper: alternately store the two snapshots and Load them back, racing
	// the atomic.Pointer against the writer's and readers' Loads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < concurrencySwapIters; i++ {
			if i%2 == 0 {
				holder.Store(dbB)
			} else {
				holder.Store(dbA)
			}
			_ = holder.Load()
		}
		// Leave a deterministic snapshot installed at the end.
		holder.Store(dbA)
	}()

	wg.Wait()

	// Post-run sanity: parse ran, so the store carries the feed's header time and
	// the read path still works without a race. This is a liveness check, not a
	// golden assertion.
	if store.FeedTime().IsZero() {
		t.Error("LiveStore.FeedTime is zero after concurrent parses — parse never committed")
	}
}

// TestConcurrency_SyntheticFeedRace mirrors the fixture test but drives parse
// with a small synthetic feed that contains a scheduled delay, a cancellation
// and an added trip. The captured fixture has no additions at our stops, so this
// case specifically exercises concurrent writes/reads of the Additions and
// Cancellations maps (parse writing them under the lock; QueryArrivals reading
// them via GetAdditions/IsCancelled) under -race.
func TestConcurrency_SyntheticFeedRace(t *testing.T) {
	dbA := makeTestDB()
	dbB := makeTestDB()

	holder := NewDB(dbA)
	poller := NewPoller("", "test", holder)

	store := withClock(poller.Store(), fixtureNow())
	now := fixtureNow()

	// Added arrival lands inside the query window so the addition read path is
	// actually traversed.
	feed := concurrencySyntheticFeed(t, now.Add(10*time.Minute).Unix())

	if err := poller.parse(feed); err != nil {
		t.Fatalf("initial parse of synthetic feed: %v", err)
	}

	const stop = "1358" // the single stop makeTestDB knows about

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < concurrencyParseIters; i++ {
			if err := poller.parse(feed); err != nil {
				t.Errorf("concurrent synthetic parse iteration %d: %v", i, err)
				return
			}
		}
	}()

	for g := 0; g < concurrencyQueryGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < concurrencyQueryIters; i++ {
				db := holder.Load()
				_ = QueryArrivals(db, store, stop, now, 120, 0, nil)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < concurrencySwapIters; i++ {
			if i%2 == 0 {
				holder.Store(dbB)
			} else {
				holder.Store(dbA)
			}
			_ = holder.Load()
		}
		holder.Store(dbA)
	}()

	wg.Wait()

	if store.FeedTime().IsZero() {
		t.Error("LiveStore.FeedTime is zero after concurrent synthetic parses")
	}
}

// concurrencySyntheticFeed builds and marshals a minimal GTFS-RT feed containing
// one scheduled delay (trip 3582_6405, seq 78), one cancellation (trip
// 3582_9999) and one added trip at stop 1358 arriving at addArrUnix. It reuses
// the ptr* helpers defined alongside the other realtime tests in this package.
func concurrencySyntheticFeed(t *testing.T, addArrUnix int64) []byte {
	t.Helper()

	schedRel := gtfsrt.TripDescriptor_SCHEDULED
	cancelRel := gtfsrt.TripDescriptor_CANCELED
	addedRel := gtfsrt.TripDescriptor_ADDED
	stopSched := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED

	delay := int32(120)
	seq78 := uint32(78)
	addSeq := uint32(1)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(uint64(fixtureFeedUnix)),
		},
		Entity: []*gtfsrt.FeedEntity{
			{
				// Scheduled trip with a live delay.
				Id: ptrString("sched1"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("3582_6405"),
						ScheduleRelationship: &schedRel,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence:         &seq78,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &delay},
						ScheduleRelationship: &stopSched,
					}},
				},
			},
			{
				// Cancelled trip — writes into the Cancellations map.
				Id: ptrString("cancel1"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("3582_9999"),
						ScheduleRelationship: &cancelRel,
					},
				},
			},
			{
				// Added trip at stop 1358 — writes into the Additions map.
				Id: ptrString("add1"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("added_trip_1"),
						RouteId:              ptrString("brand_new_route"),
						ScheduleRelationship: &addedRel,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: &addSeq,
						StopId:       ptrString("1358"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(addArrUnix)},
					}},
				},
			},
		},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal synthetic feed: %v", err)
	}
	return data
}
