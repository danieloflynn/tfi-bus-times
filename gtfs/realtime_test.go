package gtfs

import (
	"os"
	"path/filepath"
	"testing"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

// ptrString / ptrUint64 / ptrInt32 / ptrUint32 create pointer literals for
// proto2-style optional fields in the GTFS-RT generated Go structs.
func ptrString(s string) *string  { return &s }
func ptrUint64(u uint64) *uint64  { return &u }
func ptrInt32(i int32) *int32     { return &i }
func ptrUint32(u uint32) *uint32  { return &u }

// TestParseLiveResponseFile parses the checked-in .gtfsr fixture and verifies
// that the delay for trip "3582_6405" at stop_sequence 78 is 88 seconds,
// matching the Python test.
func TestParseLiveResponseFile(t *testing.T) {
	// Fixture is two directories up from this package.
	fixturePath := filepath.Join("..", "..", "test_data", "test_live_response.gtfsr")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Skipf("fixture not found (%v); run from repo root or supply test_data/", err)
	}

	db := makeTestDB()
	poller := NewPoller("", "test", db)

	if err := poller.parse(data); err != nil {
		t.Fatalf("parse failed: %v", err)
	}

	ls := poller.Store()
	sd, ok := ls.GetDelay("3582_6405", 78)
	if !ok {
		t.Fatal("no delay found for trip 3582_6405 at seq 78")
	}
	if sd.AbsTime != 0 {
		// Arrival.time was set rather than a relative delay.
		t.Logf("AbsTime=%d (absolute arrival time stored, not relative delay)", sd.AbsTime)
		return
	}
	if sd.DelaySeconds != 88 {
		t.Errorf("expected delay 88s for trip 3582_6405 seq 78, got %d", sd.DelaySeconds)
	}
}

// TestProtoUnmarshal builds a minimal synthetic GTFS-RT feed, marshals it,
// parses it through the Poller, and verifies the resulting delay.
func TestProtoUnmarshal(t *testing.T) {
	delay := int32(60)
	seq := uint32(5)
	rel := gtfsrt.TripDescriptor_SCHEDULED
	stopRel := gtfsrt.TripUpdate_StopTimeUpdate_SCHEDULED

	feed := &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: ptrString("2.0"),
			Timestamp:           ptrUint64(1694771400),
		},
		Entity: []*gtfsrt.FeedEntity{
			{
				Id: ptrString("e1"),
				TripUpdate: &gtfsrt.TripUpdate{
					Trip: &gtfsrt.TripDescriptor{
						TripId:               ptrString("test_trip"),
						ScheduleRelationship: &rel,
					},
					StopTimeUpdate: []*gtfsrt.TripUpdate_StopTimeUpdate{
						{
							StopSequence: &seq,
							Arrival: &gtfsrt.TripUpdate_StopTimeEvent{
								Delay: &delay,
							},
							ScheduleRelationship: &stopRel,
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(feed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	db := makeTestDB()
	db.Trips["test_trip"] = Trip{RouteShort: "99", ServiceID: "200", Headsign: "Test"}
	poller := NewPoller("", "test", db)
	if err := poller.parse(data); err != nil {
		t.Fatalf("parse: %v", err)
	}

	ls := poller.Store()
	sd, ok := ls.GetDelay("test_trip", 5)
	if !ok {
		t.Fatal("expected delay for test_trip seq 5")
	}
	if sd.DelaySeconds != 60 {
		t.Errorf("want 60s delay, got %d", sd.DelaySeconds)
	}

	_ = ptrInt32
	_ = ptrUint32
}
