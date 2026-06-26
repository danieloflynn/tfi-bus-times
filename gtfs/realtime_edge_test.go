package gtfs

// FR4 — Realtime parse/poll/backoff edge cases.
//
// Covers: 24h cancellation carry-over across two parses; implausible-delay
// discard (> maxDelaySeconds); skipped-stop exclusion; unknown-trip counting;
// AbsTime vs delta-Delay; added-trip arrival-vs-departure-time fallback and
// same-route dedupe; nil header / empty feed; Poll/fetch via httptest.Server
// (200 / 429 / non-200 / transport error); BackoffDuration table (exponential
// growth, 3600s cap); resolveStopID (StopIDToNumber rail path); routeShortName
// (resolved / unresolved).
//
// All helpers are prefixed "rtParse" to avoid collisions with the other
// hardening files in this package.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// rtParseCancelFeed marshals a GTFS-RT feed that cancels one trip.
func rtParseCancelFeed(t *testing.T, tripID string, feedTS uint64) []byte {
	t.Helper()
	rel := gtfsrt.TripDescriptor_CANCELED
	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(feedTS),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("c1"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString(tripID),
					ScheduleRelationship: &rel,
				},
			},
		}},
	}
	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("rtParseCancelFeed marshal: %v", err)
	}
	return data
}

// rtParseEmptyFeed marshals a GTFS-RT feed with no entity updates.
func rtParseEmptyFeed(t *testing.T, feedTS uint64) []byte {
	t.Helper()
	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(feedTS),
		},
	}
	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("rtParseEmptyFeed marshal: %v", err)
	}
	return data
}

// rtParseMinimalValidFeed marshals a small valid proto feed for use in HTTP
// handler tests. The trip does not need to be in the StaticDB because we are
// only verifying that Poll reached the parse stage, not the stored delays.
func rtParseMinimalValidFeed(t *testing.T) []byte {
	t.Helper()
	rel := gtfsrt.TripDescriptor_SCHEDULED
	stopRel := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED
	delay := int32(60)
	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("mv1"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString("rt_poll_trip"),
					ScheduleRelationship: &rel,
				},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
					StopSequence:         ptrUint32(1),
					Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &delay},
					ScheduleRelationship: &stopRel,
				}},
			},
		}},
	}
	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("rtParseMinimalValidFeed marshal: %v", err)
	}
	return data
}

// ---------------------------------------------------------------------------
// Cancellation carry-over
// ---------------------------------------------------------------------------

// TestRTParse_CancellationCarryOver verifies that a cancellation is preserved
// across a second parse when the LiveStore clock is within the 24-hour window,
// and is dropped when the clock advances past it.
//
// The LiveStore records the feed-header timestamp (feedTime) for each
// cancellation. On the next parse, old cancellations with now()-feedTime < 24h
// are copied into the new map; those at or beyond 24h are silently dropped.
func TestRTParse_CancellationCarryOver(t *testing.T) {
	const tripID = "rt_carry_trip"

	db := makeTestDB()
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	// T0 is the feed header timestamp for the initial cancellation feed.
	T0 := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	feedTS0 := uint64(T0.Unix())

	// ── Parse 1 ── feed that cancels tripID; clock at T0.
	// After this parse, Cancellations[tripID] == T0.
	poller.store.now = func() time.Time { return T0 }
	data1 := rtParseCancelFeed(t, tripID, feedTS0)
	if err := poller.parse(data1); err != nil {
		t.Fatalf("parse 1 (cancel feed): %v", err)
	}
	if !poller.Store().IsCancelled(tripID) {
		t.Fatal("trip should be cancelled immediately after parse 1")
	}

	// ── Parse 2 ── empty feed; clock at T0 + 1h (within 24h).
	// now()-feedTime = 1h < 24h → carry-over must keep the cancellation.
	within := T0.Add(1 * time.Hour)
	poller.store.now = func() time.Time { return within }
	data2 := rtParseEmptyFeed(t, feedTS0+3600)
	if err := poller.parse(data2); err != nil {
		t.Fatalf("parse 2 (empty feed, within 24h): %v", err)
	}
	if !poller.Store().IsCancelled(tripID) {
		t.Error("cancellation should carry over within the 24h window")
	}

	// ── Parse 3 ── empty feed; clock at T0 + 25h (past the 24h window).
	// now()-feedTime = 25h >= 24h → the old entry must be dropped from the new map.
	expired := T0.Add(25 * time.Hour)
	poller.store.now = func() time.Time { return expired }
	data3 := rtParseEmptyFeed(t, feedTS0+uint64(25*3600))
	if err := poller.parse(data3); err != nil {
		t.Fatalf("parse 3 (empty feed, expired): %v", err)
	}
	if poller.Store().IsCancelled(tripID) {
		t.Error("cancellation should be dropped once the 24h window has passed")
	}
}

// ---------------------------------------------------------------------------
// Implausible delay discard
// ---------------------------------------------------------------------------

// TestRTParse_ImplausibleDelayDiscard verifies that arrival delays outside
// the [-maxDelaySeconds, maxDelaySeconds] range are silently discarded, while
// a delay exactly at the boundary is kept.
func TestRTParse_ImplausibleDelayDiscard(t *testing.T) {
	db := makeTestDB()
	db.Trips["rt_implausible_trip"] = Trip{RouteShort: "IM", ServiceID: "200", Headsign: "Test"}
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	rel := gtfsrt.TripDescriptor_SCHEDULED
	stopRel := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED

	limitLate := int32(maxDelaySeconds)     // exactly at limit → kept
	overLate := int32(maxDelaySeconds + 1)  // one over → discarded
	overEarly := int32(-maxDelaySeconds - 1) // implausibly early → discarded

	seq1, seq2, seq3 := uint32(1), uint32(2), uint32(3)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("impl1"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString("rt_implausible_trip"),
					ScheduleRelationship: &rel,
				},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{
					{
						StopSequence:         &seq1,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &overLate},
						ScheduleRelationship: &stopRel,
					},
					{
						StopSequence:         &seq2,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &limitLate},
						ScheduleRelationship: &stopRel,
					},
					{
						StopSequence:         &seq3,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &overEarly},
						ScheduleRelationship: &stopRel,
					},
				},
			},
		}},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Inspect the raw delays slice directly: the binary-search GetDelay returns a
	// floor match (largest seq ≤ requested), so we can't distinguish "seq N
	// absent" from "seq N has a floor match" using GetDelay alone.
	ls := poller.Store()
	ls.mu.RLock()
	delays := ls.Delays["rt_implausible_trip"]
	ls.mu.RUnlock()

	// Only seq 2 (at exactly maxDelaySeconds) must be present.
	if len(delays) != 1 {
		t.Fatalf("expected exactly 1 stored delay (seq 2 at limit), got %d: %v", len(delays), delays)
	}
	if delays[0].StopSequence != 2 {
		t.Errorf("stored delay should be for seq 2, got seq %d", delays[0].StopSequence)
	}
	if delays[0].DelaySeconds != int32(maxDelaySeconds) {
		t.Errorf("delay at maxDelaySeconds: want %d, got %d", maxDelaySeconds, delays[0].DelaySeconds)
	}
}

// ---------------------------------------------------------------------------
// Skipped stop exclusion
// ---------------------------------------------------------------------------

// TestRTParse_SkippedStopExcluded verifies that a stop-time update whose
// ScheduleRelationship is SKIPPED is not stored in the delay map.
func TestRTParse_SkippedStopExcluded(t *testing.T) {
	db := makeTestDB()
	db.Trips["rt_skip_trip"] = Trip{RouteShort: "SK", ServiceID: "200", Headsign: "Skip Test"}
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	rel := gtfsrt.TripDescriptor_SCHEDULED
	skipped := gtfsrt.TripUpdate_StopTimeUpdate_SKIPPED
	scheduled := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED
	delay := int32(30)
	seqSkip := uint32(5)
	seqKeep := uint32(6)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("skip1"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString("rt_skip_trip"),
					ScheduleRelationship: &rel,
				},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{
					{
						StopSequence:         &seqSkip,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &delay},
						ScheduleRelationship: &skipped,
					},
					{
						StopSequence:         &seqKeep,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &delay},
						ScheduleRelationship: &scheduled,
					},
				},
			},
		}},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if _, ok := poller.Store().GetDelay("rt_skip_trip", int(seqSkip)); ok {
		t.Error("SKIPPED stop_time_update must not be stored in the delay map")
	}
	if _, ok := poller.Store().GetDelay("rt_skip_trip", int(seqKeep)); !ok {
		t.Error("SCHEDULED stop_time_update must be stored in the delay map")
	}
}

// ---------------------------------------------------------------------------
// Absolute arrival time vs delta delay
// ---------------------------------------------------------------------------

// TestRTParse_AbsTimeVsDelta verifies the two arrival-time storage branches:
//   - when Arrival.Time is non-zero it is stored as StopDelay.AbsTime and
//     DelaySeconds is zero;
//   - when Arrival.Time is zero and Arrival.Delay is set, DelaySeconds is
//     stored and AbsTime is zero.
func TestRTParse_AbsTimeVsDelta(t *testing.T) {
	db := makeTestDB()
	db.Trips["rt_abs_trip"] = Trip{RouteShort: "AB", ServiceID: "200", Headsign: "AbsTime"}
	db.Trips["rt_delta_trip"] = Trip{RouteShort: "DE", ServiceID: "200", Headsign: "Delta"}
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	absTS := int64(1694775000)
	deltaS := int32(180)
	rel := gtfsrt.TripDescriptor_SCHEDULED
	stopRel := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED
	seqAbs := uint32(10)
	seqDelta := uint32(20)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{
			{
				Id: ptrString("abs1"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_abs_trip"),
						ScheduleRelationship: &rel,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence:         &seqAbs,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(absTS)},
						ScheduleRelationship: &stopRel,
					}},
				},
			},
			{
				Id: ptrString("delta1"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_delta_trip"),
						ScheduleRelationship: &rel,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence:         &seqDelta,
						Arrival:              &gtfsrt.TripUpdate_StopTimeEvent{Delay: &deltaS},
						ScheduleRelationship: &stopRel,
					}},
				},
			},
		},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// AbsTime branch.
	sdAbs, ok := poller.Store().GetDelay("rt_abs_trip", int(seqAbs))
	if !ok {
		t.Fatal("expected delay entry for rt_abs_trip")
	}
	if sdAbs.AbsTime != absTS {
		t.Errorf("AbsTime: want %d, got %d", absTS, sdAbs.AbsTime)
	}
	if sdAbs.DelaySeconds != 0 {
		t.Errorf("DelaySeconds should be 0 when AbsTime is used, got %d", sdAbs.DelaySeconds)
	}

	// Delta branch.
	sdDelta, ok := poller.Store().GetDelay("rt_delta_trip", int(seqDelta))
	if !ok {
		t.Fatal("expected delay entry for rt_delta_trip")
	}
	if sdDelta.DelaySeconds != deltaS {
		t.Errorf("DelaySeconds: want %d, got %d", deltaS, sdDelta.DelaySeconds)
	}
	if sdDelta.AbsTime != 0 {
		t.Errorf("AbsTime should be 0 when Delay is used, got %d", sdDelta.AbsTime)
	}
}

// ---------------------------------------------------------------------------
// Added-trip: arrival-time fallback to departure
// ---------------------------------------------------------------------------

// TestRTParse_AddedTripArrivalFallback verifies the arrival-vs-departure
// fallback for added trips. Three sub-cases use separate stops so that the
// same-route dedupe logic (which only triggers within the same stop) does not
// interfere:
//   - stop "rt_fb_arr" receives only an arrival time → stored as-is;
//   - stop "rt_fb_dep" receives only a departure time → departure used as arrival;
//   - stop "rt_fb_none" receives neither → stop_time_update silently skipped.
func TestRTParse_AddedTripArrivalFallback(t *testing.T) {
	db := makeTestDB()
	// Register each stop in StopNames so resolveStopID returns its id directly.
	db.StopNames["rt_fb_arr"] = "Fallback Arr Stop"
	db.StopNames["rt_fb_dep"] = "Fallback Dep Stop"
	db.StopNames["rt_fb_none"] = "Fallback None Stop"
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	arrTS := int64(1694771600)
	departTS := int64(1694771700)
	added := gtfsrt.TripDescriptor_ADDED

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("fb_trip"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString("rt_fb_added_trip"),
					RouteId:              ptrString("route49"),
					ScheduleRelationship: &added,
				},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{
					{
						// Different stop from seq 2/3 to avoid same-route dedupe.
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_fb_arr"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(arrTS)},
					},
					{
						// No arrival, has departure — fallback must fire.
						// Different stop from seq 1 to avoid same-route dedupe.
						StopSequence: ptrUint32(2),
						StopId:       ptrString("rt_fb_dep"),
						Departure:    &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(departTS)},
					},
					{
						// Neither arrival nor departure → must be silently skipped.
						StopSequence: ptrUint32(3),
						StopId:       ptrString("rt_fb_none"),
					},
				},
			},
		}},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// seq 1: arrival time stored directly.
	arrAdds := poller.Store().GetAdditions("rt_fb_arr")
	if len(arrAdds) != 1 {
		t.Fatalf("arrival branch: expected 1 addition at rt_fb_arr, got %d", len(arrAdds))
	}
	if arrAdds[0].ArrivalTime.Unix() != arrTS {
		t.Errorf("arrival branch: want %d, got %d", arrTS, arrAdds[0].ArrivalTime.Unix())
	}

	// seq 2: departure time used as arrival (fallback).
	depAdds := poller.Store().GetAdditions("rt_fb_dep")
	if len(depAdds) != 1 {
		t.Fatalf("departure fallback: expected 1 addition at rt_fb_dep, got %d", len(depAdds))
	}
	if depAdds[0].ArrivalTime.Unix() != departTS {
		t.Errorf("departure fallback: want %d, got %d", departTS, depAdds[0].ArrivalTime.Unix())
	}

	// seq 3: no time → silently skipped.
	noneAdds := poller.Store().GetAdditions("rt_fb_none")
	if len(noneAdds) != 0 {
		t.Errorf("neither-time branch: expected 0 additions at rt_fb_none, got %d", len(noneAdds))
	}
}

// ---------------------------------------------------------------------------
// Added-trip: same-route dedupe
// ---------------------------------------------------------------------------

// TestRTParse_AddedTripSameRouteDedupe verifies that when two added-trip
// entities carry the same route to the same stop and the second entity has a
// LATER arrival time, the first (earlier) entry is replaced.
func TestRTParse_AddedTripSameRouteDedupe(t *testing.T) {
	db := makeTestDB()
	db.StopNames["rt_ded_stop"] = "Dedup Stop"
	db.RouteShortNames["rt_ded_route"] = "DED"
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	earlierTS := int64(1694771500)
	laterTS := int64(1694775000)
	added := gtfsrt.TripDescriptor_ADDED

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{
			{
				// First entity: earlier arrival.
				Id: ptrString("ded_trip_a"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_ded_trip_1"),
						RouteId:              ptrString("rt_ded_route"),
						ScheduleRelationship: &added,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_ded_stop"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(earlierTS)},
					}},
				},
			},
			{
				// Second entity: later arrival, same route → earlier must be dropped.
				Id: ptrString("ded_trip_b"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_ded_trip_2"),
						RouteId:              ptrString("rt_ded_route"),
						ScheduleRelationship: &added,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_ded_stop"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(laterTS)},
					}},
				},
			},
		},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	adds := poller.Store().GetAdditions("rt_ded_stop")
	if len(adds) != 1 {
		t.Fatalf("same-route dedupe (earlier→later order): want 1 addition, got %d", len(adds))
	}
	if adds[0].ArrivalTime.Unix() != laterTS {
		t.Errorf("dedupe should keep the later arrival (%d), got %d", laterTS, adds[0].ArrivalTime.Unix())
	}
	if adds[0].RouteShortName != "DED" {
		t.Errorf("route short name: want %q, got %q", "DED", adds[0].RouteShortName)
	}
}

// TestRTParse_AddedTripDedupOrderSensitive documents the current behaviour when
// a second added entity for the same route has an EARLIER arrival than the first:
// the dedupe only drops the OLD entry when the new one arrives LATER (the
// condition is a.ArrivalTime.Before(arr.ArrivalTime)), so when the new entry
// arrives earlier, both entries are kept.
//
// NOTE: this order-sensitivity is a suspected bug — see suspectedBugs field in
// the structured output. The test asserts CURRENT behaviour so the suite stays
// green; a fix would change the assertion to len(adds)==1.
func TestRTParse_AddedTripDedupOrderSensitive(t *testing.T) {
	db := makeTestDB()
	db.StopNames["rt_ods_stop"] = "ODS Stop"
	db.RouteShortNames["rt_ods_route"] = "ODS"
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	laterTS := int64(1694775000) // first entity: LATER arrival
	earlierTS := int64(1694771500) // second entity: EARLIER arrival
	added := gtfsrt.TripDescriptor_ADDED

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{
			{
				Id: ptrString("ods_trip_a"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_ods_trip_1"),
						RouteId:              ptrString("rt_ods_route"),
						ScheduleRelationship: &added,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_ods_stop"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(laterTS)},
					}},
				},
			},
			{
				Id: ptrString("ods_trip_b"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_ods_trip_2"),
						RouteId:              ptrString("rt_ods_route"),
						ScheduleRelationship: &added,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_ods_stop"),
						// Earlier than the existing entry: dedupe condition
						// (existing.Before(new)) is false, so OLD is NOT dropped.
						Arrival: &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(earlierTS)},
					}},
				},
			},
		},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	adds := poller.Store().GetAdditions("rt_ods_stop")
	// Current behaviour: both entries kept (the second one does not displace the
	// first because the first is not "older" than the second).
	if len(adds) != 2 {
		t.Errorf("order-sensitive dedupe (later→earlier): current behaviour keeps both entries; got %d", len(adds))
	}
}

// ---------------------------------------------------------------------------
// Unknown trip
// ---------------------------------------------------------------------------

// TestRTParse_UnknownTripIgnored verifies that a SCHEDULED trip whose trip_id
// is not present in db.Trips is counted as unknown and not stored.
func TestRTParse_UnknownTripIgnored(t *testing.T) {
	db := makeTestDB()
	// "rt_ghost_trip" is deliberately absent from db.Trips.
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	rel := gtfsrt.TripDescriptor_SCHEDULED
	stopRel := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED
	delay := int32(60)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("ghost1"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString("rt_ghost_trip"),
					ScheduleRelationship: &rel,
				},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
					StopSequence:         ptrUint32(10),
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
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := poller.Store().GetDelay("rt_ghost_trip", 10); ok {
		t.Error("unknown trip must be ignored — delay must not be stored")
	}
}

// ---------------------------------------------------------------------------
// Nil header / empty feed
// ---------------------------------------------------------------------------

// TestRTParse_ZeroTimestampHeader verifies that a FeedMessage whose Header
// carries a zero timestamp is stored as feedTime = time.Unix(0, 0).
//
// The `if feed.Header != nil` guard in parse is defensive code for the
// nil-header case: the GTFS-RT proto2 schema marks Header as required, so
// google.golang.org/protobuf/proto.Unmarshal (v1.33+) returns an error for any
// message that omits it. In practice the guard is therefore unreachable via
// normal proto.Unmarshal. This test covers the observable equivalent: a feed
// with an explicit header whose Timestamp field is unset (zero), which sets
// feedTime = time.Unix(0, 0) through the same assignment path.
func TestRTParse_ZeroTimestampHeader(t *testing.T) {
	db := makeTestDB()
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	// Feed with a present but zero-timestamp header (Timestamp not set → 0).
	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			// Timestamp intentionally omitted → GetTimestamp() returns 0.
		},
	}
	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse with zero-timestamp header: %v", err)
	}

	// feedTS = 0 → feedTime = time.Unix(0, 0).
	if !poller.Store().FeedTime().Equal(time.Unix(0, 0)) {
		t.Errorf("zero-timestamp header: FeedTime should be Unix epoch, got %v", poller.Store().FeedTime())
	}
}

// TestRTParse_EmptyFeed verifies that an empty entity list parses cleanly and
// leaves all LiveStore maps empty.
func TestRTParse_EmptyFeed(t *testing.T) {
	db := makeTestDB()
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	data := rtParseEmptyFeed(t, 1694771400)
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse empty feed: %v", err)
	}

	ls := poller.Store()
	ls.mu.RLock()
	delayLen := len(ls.Delays)
	cancelLen := len(ls.Cancellations)
	addLen := len(ls.Additions)
	ls.mu.RUnlock()

	if delayLen != 0 {
		t.Errorf("empty feed: Delays should be empty, got %d entries", delayLen)
	}
	if cancelLen != 0 {
		t.Errorf("empty feed: Cancellations should be empty, got %d entries", cancelLen)
	}
	if addLen != 0 {
		t.Errorf("empty feed: Additions should be empty, got %d entries", addLen)
	}
}

// ---------------------------------------------------------------------------
// Poll / fetch (via httptest.Server)
// ---------------------------------------------------------------------------

// TestRTParse_Poll200ResetsRateLimit verifies that a 200 response triggers a
// successful parse and resets rateLimitCount to 0.
func TestRTParse_Poll200ResetsRateLimit(t *testing.T) {
	db := makeTestDB()
	// "rt_poll_trip" is referenced in rtParseMinimalValidFeed but absent from
	// the StaticDB; that is fine — parse silently counts it as unknown, which
	// exercises the fetch→parse path we care about here.
	holder := NewDB(db)

	feedData := rtParseMinimalValidFeed(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(feedData)
	}))
	defer srv.Close()

	poller := NewPoller(srv.URL, "testkey", holder)
	poller.rateLimitCount = 5 // simulate prior rate-limit back-off

	count := poller.Poll()
	if count != 0 {
		t.Errorf("200 response should reset rateLimitCount to 0, got %d", count)
	}
	// LastPollTime must have been updated (the only observable side-effect of a
	// successful poll beyond the delay map).
	if poller.Store().PollTime().IsZero() {
		t.Error("LastPollTime should be set after a successful 200 poll")
	}
}

// TestRTParse_Poll429IncrementsCount verifies that each 429 response increments
// rateLimitCount by 1 and that Poll returns the new count.
func TestRTParse_Poll429IncrementsCount(t *testing.T) {
	db := makeTestDB()
	holder := NewDB(db)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	poller := NewPoller(srv.URL, "testkey", holder)

	// First 429.
	count := poller.Poll()
	if count != 1 {
		t.Errorf("first 429: want rateLimitCount=1, got %d", count)
	}
	// Second 429.
	count = poller.Poll()
	if count != 2 {
		t.Errorf("second 429: want rateLimitCount=2, got %d", count)
	}
}

// TestRTParse_PollNon200Error verifies that a non-200/non-429 status (e.g. 503)
// causes fetch to return an error and Poll returns the unchanged rateLimitCount.
func TestRTParse_PollNon200Error(t *testing.T) {
	db := makeTestDB()
	holder := NewDB(db)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // 503
	}))
	defer srv.Close()

	poller := NewPoller(srv.URL, "testkey", holder)
	poller.rateLimitCount = 3 // must remain unchanged

	count := poller.Poll()
	if count != 3 {
		t.Errorf("non-200 error: want rateLimitCount unchanged at 3, got %d", count)
	}
}

// TestRTParse_PollTransportError verifies that a network-level failure (e.g.
// connection refused because the server was closed) causes fetch to return an
// error and Poll returns the unchanged rateLimitCount without panicking.
func TestRTParse_PollTransportError(t *testing.T) {
	db := makeTestDB()
	holder := NewDB(db)

	// Start and immediately close the server so all connections are refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvURL := srv.URL
	srv.Close()

	poller := NewPoller(srvURL, "testkey", holder)
	poller.rateLimitCount = 2

	count := poller.Poll()
	if count != 2 {
		t.Errorf("transport error: want rateLimitCount unchanged at 2, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// BackoffDuration table
// ---------------------------------------------------------------------------

// TestRTParse_BackoffDuration exercises BackoffDuration for a range of
// rateLimitCount values, verifying exponential growth (base × 2^(count-1))
// and the hard 3600s cap.
func TestRTParse_BackoffDuration(t *testing.T) {
	// baseSec × 2^(rateLimitCount-1), capped at 3600s.
	cases := []struct {
		name           string
		rateLimitCount int
		baseSec        int
		want           time.Duration
	}{
		{"zero_count_no_backoff", 0, 10, 0},
		{"count1_base10", 1, 10, 10 * time.Second},  // 10 × 2^0 = 10
		{"count2_base10", 2, 10, 20 * time.Second},  // 10 × 2^1 = 20
		{"count3_base10", 3, 10, 40 * time.Second},  // 10 × 2^2 = 40
		{"count4_base10", 4, 10, 80 * time.Second},  // 10 × 2^3 = 80
		{"count5_base60", 5, 60, 960 * time.Second},  // 60 × 2^4 = 960
		{"count6_base60", 6, 60, 1920 * time.Second}, // 60 × 2^5 = 1920
		// count=7, base=60: 60 × 2^6 = 3840 > 3600 → capped.
		{"count7_base60_capped", 7, 60, 3600 * time.Second},
		// Large count must still cap at 3600s.
		{"count20_base60_capped", 20, 60, 3600 * time.Second},
		// base=1 with large count.
		{"count13_base1_capped", 13, 1, 3600 * time.Second}, // 1 × 2^12 = 4096 > 3600
		{"count12_base1_not_capped", 12, 1, 2048 * time.Second}, // 1 × 2^11 = 2048
	}

	db := makeTestDB()
	holder := NewDB(db)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			poller := NewPoller("", "testkey", holder)
			poller.rateLimitCount = c.rateLimitCount
			got := poller.BackoffDuration(c.baseSec)
			if got != c.want {
				t.Errorf("BackoffDuration(base=%d) with rateLimitCount=%d: want %v, got %v",
					c.baseSec, c.rateLimitCount, c.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveStopID
// ---------------------------------------------------------------------------

// TestRTParse_ResolveStopIDRailPath exercises all three branches of the
// resolveStopID helper:
//   1. stop_id directly in StopNames → returned unchanged (bus stop path);
//   2. stop_id in StopIDToNumber → mapped to stop_number (rail path);
//   3. stop_id in neither map → returned as-is (fallback for unknown stops).
func TestRTParse_ResolveStopIDRailPath(t *testing.T) {
	db := &StaticDB{
		StopNames:      map[string]string{"42": "A Bus Stop"},
		StopIDToNumber: map[string]string{"8400_RAIL": "999126"},
	}

	// Branch 1: direct StopNames hit (bus stop path).
	if got := resolveStopID(db, "42"); got != "42" {
		t.Errorf("StopNames branch: want %q, got %q", "42", got)
	}
	// Branch 2: StopIDToNumber hit (rail path).
	if got := resolveStopID(db, "8400_RAIL"); got != "999126" {
		t.Errorf("StopIDToNumber branch: want %q, got %q", "999126", got)
	}
	// Branch 3: fallback — identity return.
	if got := resolveStopID(db, "totally_unknown"); got != "totally_unknown" {
		t.Errorf("fallback branch: want identity %q, got %q", "totally_unknown", got)
	}
}

// TestRTParse_ResolveStopIDRailViaFeedParse verifies the StopIDToNumber path
// end-to-end: an added trip referencing a raw GTFS stop_id (not the stop_code)
// is correctly filed under the resolved stop_number in the Additions map.
func TestRTParse_ResolveStopIDRailViaFeedParse(t *testing.T) {
	db := makeTestDB()
	// Map raw GTFS id → stop_number (rail pattern).
	db.StopIDToNumber = map[string]string{"RAIL_99999": "999126"}
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	added := gtfsrt.TripDescriptor_ADDED
	arrTS := int64(1694771900)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{{
			Id: ptrString("rail_add"),
			TripUpdate: &gtfsrt.TripUpdate{
				Trip: &gtfsrt.TripDescriptor{
					TripId:               ptrString("rt_rail_added_trip"),
					RouteId:              ptrString("some_route"),
					ScheduleRelationship: &added,
				},
				StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
					StopSequence: ptrUint32(1),
					StopId:       ptrString("RAIL_99999"), // raw GTFS id, not stop_code
					Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(arrTS)},
				}},
			},
		}},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// The addition must be stored under the resolved stop_number, not the raw id.
	adds := poller.Store().GetAdditions("999126")
	if len(adds) != 1 {
		t.Fatalf("rail path: expected 1 addition under resolved stop 999126, got %d", len(adds))
	}
	if adds[0].ArrivalTime.Unix() != arrTS {
		t.Errorf("arrival time: want %d, got %d", arrTS, adds[0].ArrivalTime.Unix())
	}
	// The addition must NOT be under the raw id.
	rawAdds := poller.Store().GetAdditions("RAIL_99999")
	if len(rawAdds) != 0 {
		t.Errorf("addition should not be stored under raw GTFS stop_id; got %d entries", len(rawAdds))
	}
}

// ---------------------------------------------------------------------------
// routeShortName
// ---------------------------------------------------------------------------

// TestRTParse_RouteShortName tests both branches of routeShortName: a route
// found in the static data (RouteResolved=true) and one that is not
// (RouteResolved=false, raw routeID returned).
func TestRTParse_RouteShortName(t *testing.T) {
	db := &StaticDB{
		RouteShortNames: map[string]string{"rt_route_X": "X"},
	}

	// Resolved: routeID is present in the static feed.
	name, resolved := routeShortName(db, "rt_route_X")
	if !resolved {
		t.Error("route in RouteShortNames must be resolved (true)")
	}
	if name != "X" {
		t.Errorf("resolved route: want %q, got %q", "X", name)
	}

	// Unresolved: routeID is not in the static feed (e.g. a new disruption route).
	name, resolved = routeShortName(db, "brand_new_disruption_route")
	if resolved {
		t.Error("absent route must not be resolved (false)")
	}
	if name != "brand_new_disruption_route" {
		t.Errorf("unresolved route: want raw id %q, got %q", "brand_new_disruption_route", name)
	}
}

// TestRTParse_RouteShortNameViaFeedParse verifies that RouteResolved is set
// correctly on Addition entries parsed from a synthetic feed: one added trip
// references a known route_id and another references an unknown one.
func TestRTParse_RouteShortNameViaFeedParse(t *testing.T) {
	db := makeTestDB()
	// makeTestDB already has "route49" → "49" in RouteShortNames.
	db.StopNames["rt_rsn_stop"] = "RSN Stop"
	holder := NewDB(db)
	poller := NewPoller("", "test", holder)

	added := gtfsrt.TripDescriptor_ADDED
	ts1 := int64(1694771600)
	ts2 := int64(1694771700)

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{
			{
				// Known route_id → RouteResolved=true, RouteShortName="49".
				Id: ptrString("rsn_known"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_rsn_trip_1"),
						RouteId:              ptrString("route49"),
						ScheduleRelationship: &added,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_rsn_stop"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(ts1)},
					}},
				},
			},
			{
				// Unknown route_id → RouteResolved=false, RouteShortName=raw id.
				Id: ptrString("rsn_unknown"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("rt_rsn_trip_2"),
						RouteId:              ptrString("rt_disruption_99"),
						ScheduleRelationship: &added,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{{
						StopSequence: ptrUint32(1),
						StopId:       ptrString("rt_rsn_stop"),
						Arrival:      &gtfsrt.TripUpdate_StopTimeEvent{Time: ptrInt64(ts2)},
					}},
				},
			},
		},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	adds := poller.Store().GetAdditions("rt_rsn_stop")
	if len(adds) != 2 {
		t.Fatalf("expected 2 additions (one known, one unknown route), got %d", len(adds))
	}

	for _, a := range adds {
		switch a.ArrivalTime.Unix() {
		case ts1:
			if !a.RouteResolved {
				t.Error("addition for known route49 must have RouteResolved=true")
			}
			if a.RouteShortName != "49" {
				t.Errorf("known route short name: want %q, got %q", "49", a.RouteShortName)
			}
		case ts2:
			if a.RouteResolved {
				t.Error("addition for unknown route must have RouteResolved=false")
			}
			if a.RouteShortName != "rt_disruption_99" {
				t.Errorf("unresolved route short name: want raw id %q, got %q", "rt_disruption_99", a.RouteShortName)
			}
		default:
			t.Errorf("unexpected addition with arrival time %d", a.ArrivalTime.Unix())
		}
	}
}
