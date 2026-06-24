package gtfs

import (
	"testing"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// TestDBHolder_Swap verifies the atomic holder returns whatever was last stored.
func TestDBHolder_Swap(t *testing.T) {
	a := &StaticDB{Trips: map[string]Trip{"a": {}}}
	b := &StaticDB{Trips: map[string]Trip{"b": {}}}

	h := NewDB(a)
	if h.Load() != a {
		t.Fatal("Load did not return the initial DB")
	}
	h.Store(b)
	if h.Load() != b {
		t.Fatal("Load did not return the swapped DB")
	}
}

// feedForTrip builds a minimal one-trip GTFS-RT feed with a 60s arrival delay.
func feedForTrip(t *testing.T, tripID string) []byte {
	t.Helper()
	delay := int32(60)
	seq := uint32(5)
	rel := gtfsrt.TripDescriptor_SCHEDULED
	stopRel := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED
	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("e1"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{TripId: ptrString(tripID), ScheduleRelationship: &rel},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
					StopSequence:         &seq,
					Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &delay},
					ScheduleRelationship: &stopRel,
				}},
			},
		}},
	}
	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// TestPoller_PicksUpRefreshedDB is the regression test for the "everything shows
// as scheduled after a week" bug. The realtime feed references trip IDs from the
// *current* static feed; when TFI republishes the static feed those IDs change.
// If the poller held a fixed (now-stale) StaticDB it would treat every update as
// unknown and drop it. With the atomic holder, swapping in a rebuilt DB makes the
// new trip IDs match again — without recreating the poller.
func TestPoller_PicksUpRefreshedDB(t *testing.T) {
	// Old static data: knows "old_trip" only.
	oldDB := makeTestDB()
	oldDB.Trips["old_trip"] = Trip{RouteShort: "1", ServiceID: "180", Headsign: "Old"}

	holder := NewDB(oldDB)
	poller := NewPoller("", "test", holder)

	// Feed now uses the new trip ID (as it would after an upstream feed refresh).
	feed := feedForTrip(t, "new_trip")

	if err := poller.parse(feed); err != nil {
		t.Fatalf("parse (stale DB): %v", err)
	}
	if _, ok := poller.Store().GetDelay("new_trip", 5); ok {
		t.Fatal("stale DB should not have matched new_trip — delay unexpectedly present")
	}

	// Simulate a background static refresh: rebuilt DB knows "new_trip".
	newDB := makeTestDB()
	newDB.Trips["new_trip"] = Trip{RouteShort: "1", ServiceID: "180", Headsign: "New"}
	holder.Store(newDB)

	if err := poller.parse(feed); err != nil {
		t.Fatalf("parse (refreshed DB): %v", err)
	}
	sd, ok := poller.Store().GetDelay("new_trip", 5)
	if !ok {
		t.Fatal("refreshed DB should have matched new_trip — delay missing")
	}
	if sd.DelaySeconds != 60 {
		t.Errorf("want 60s delay, got %d", sd.DelaySeconds)
	}
}
