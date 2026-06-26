// Command inspectfeed parses a captured GTFS-realtime protobuf and prints its
// header timestamp and a profile of its entities. Used once to pin the canonical
// "test now" for the fixture-based test suite. Run:
//
//	go run ./tools/inspectfeed gtfs/testdata/tripupdates.gtfsr
package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: inspectfeed <feed.gtfsr>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	feed := &gtfsrt.FeedMessage{}
	if err := proto.Unmarshal(data, feed); err != nil {
		fmt.Fprintln(os.Stderr, "unmarshal:", err)
		os.Exit(1)
	}

	ts := int64(feed.GetHeader().GetTimestamp())
	dublin, _ := time.LoadLocation("Europe/Dublin")
	fmt.Println("=== HEADER ===")
	fmt.Println("gtfs_realtime_version:", feed.GetHeader().GetGtfsRealtimeVersion())
	fmt.Println("incrementality:       ", feed.GetHeader().GetIncrementality())
	fmt.Printf("timestamp_unix:        %d\n", ts)
	fmt.Printf("timestamp_utc:         %s\n", time.Unix(ts, 0).UTC().Format(time.RFC3339))
	if dublin != nil {
		fmt.Printf("timestamp_dublin:      %s\n", time.Unix(ts, 0).In(dublin).Format(time.RFC3339))
	}

	var scheduled, added, cancelled, other, withArrival, withDelay, withAbs int
	relCount := map[int32]int{}
	stopIDs := map[string]int{}
	routeIDs := map[string]int{}
	for _, e := range feed.Entity {
		tu := e.GetTripUpdate()
		if tu == nil {
			continue
		}
		rel := int32(tu.GetTrip().GetScheduleRelationship())
		relCount[rel]++
		switch rel {
		case 0:
			scheduled++
		case 1:
			added++
		case 3:
			cancelled++
		default:
			other++
		}
		routeIDs[tu.GetTrip().GetRouteId()]++
		for _, stu := range tu.StopTimeUpdate {
			stopIDs[stu.GetStopId()]++
			if stu.GetArrival() != nil {
				withArrival++
				if stu.GetArrival().GetTime() != 0 {
					withAbs++
				}
				if stu.GetArrival().GetDelay() != 0 {
					withDelay++
				}
			}
		}
	}

	fmt.Println("\n=== ENTITY PROFILE ===")
	fmt.Println("total_entities:  ", len(feed.Entity))
	fmt.Println("scheduled:       ", scheduled)
	fmt.Println("added:           ", added)
	fmt.Println("cancelled:       ", cancelled)
	fmt.Println("other_rel:       ", other)
	fmt.Println("distinct_routes: ", len(routeIDs))
	fmt.Println("distinct_stops:  ", len(stopIDs))
	fmt.Println("stop_time_updates_with_arrival:", withArrival)
	fmt.Println("  ...with absolute time:       ", withAbs)
	fmt.Println("  ...with non-zero delay:      ", withDelay)

	// A few sample stop_ids so we can pick which to trim the fixtures to.
	type kv struct {
		k string
		n int
	}
	var samples []kv
	for k, n := range stopIDs {
		samples = append(samples, kv{k, n})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].n > samples[j].n })
	fmt.Println("\n=== TOP 15 STOP IDS (by stop_time_update count) ===")
	for i := 0; i < len(samples) && i < 15; i++ {
		fmt.Printf("  %-12s %d\n", samples[i].k, samples[i].n)
	}
}
