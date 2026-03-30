package gtfs

import (
	"testing"
	"time"
)

// makeTestDB returns a minimal StaticDB for testing.
// It contains one stop ("1358"), two services, and a handful of trips/stop_times
// chosen to match the known Python test values.
func makeTestDB() *StaticDB {
	// All times are in IST (Europe/Dublin) = UTC+1 on 2023-09-15.
	// We use UTC for storage; callers pass a UTC time to QueryArrivals.

	db := &StaticDB{
		StopTimes:       make(map[string]map[int][]StopTime),
		Trips:           make(map[string]Trip),
		Services:        make(map[string]Service),
		Exceptions:      make(map[string]int),
		StopNames:       map[string]string{"1358": "Dame St / College Green"},
		RouteShortNames: map[string]string{"route68": "68", "route49": "49", "route27": "27"},
		SchemaVer:       schemaVer,
	}

	// Service 180: runs only on 2023-09-15 (added via calendar_dates).
	// calendar.txt entry with no valid day range; exception type 1 forces it on.
	db.Services["180"] = Service{
		StartDate: mustDate("2023-09-15"),
		EndDate:   mustDate("2023-09-15"),
		Days:      [7]bool{false, false, false, false, true, false, false}, // Friday
	}

	// Service 200: regular service, Mon–Fri.
	db.Services["200"] = Service{
		StartDate: mustDate("2023-09-01"),
		EndDate:   mustDate("2023-12-31"),
		Days:      [7]bool{true, true, true, true, true, false, false},
	}

	// Trip: 3582_11643 — route 49, service 180, stop 1358 at 09:24:16
	db.Trips["3582_11643"] = Trip{RouteShort: "49", ServiceID: "180", Headsign: "Dún Laoghaire"}
	addStopTime(db, "1358", StopTime{TripID: "3582_11643", ArrivalSecs: 9*3600 + 24*60 + 16, StopSequence: 30})

	// Trip: route 68, service 200, stop 1358 at 09:15:50
	db.Trips["3582_9999"] = Trip{RouteShort: "68", ServiceID: "200", Headsign: "Dún Laoghaire"}
	addStopTime(db, "1358", StopTime{TripID: "3582_9999", ArrivalSecs: 9*3600 + 15*60 + 50, StopSequence: 45})

	// Trip: 3582_6405 — route 27, service 200, stop 1358 (delay test uses seq 78)
	db.Trips["3582_6405"] = Trip{RouteShort: "27", ServiceID: "200", Headsign: "Portmarnock"}
	addStopTime(db, "1358", StopTime{TripID: "3582_6405", ArrivalSecs: 9*3600 + 40*60, StopSequence: 78})

	return db
}

func addStopTime(db *StaticDB, stop string, st StopTime) {
	hour := (st.ArrivalSecs / 3600) % 24
	if db.StopTimes[stop] == nil {
		db.StopTimes[stop] = make(map[int][]StopTime)
	}
	db.StopTimes[stop][hour] = append(db.StopTimes[stop][hour], st)
}

func mustDate(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

// TestIsServiceActive verifies the calendar check logic.
func TestIsServiceActive(t *testing.T) {
	db := makeTestDB()

	friday := mustDate("2023-09-15") // Friday
	saturday := mustDate("2023-09-16")
	sunday := mustDate("2023-09-17")

	// Service 200: Mon–Fri, 2023-09-01 to 2023-12-31.
	if !IsServiceActive(db, "200", friday) {
		t.Error("service 200 should be active on Friday")
	}
	if IsServiceActive(db, "200", saturday) {
		t.Error("service 200 should NOT be active on Saturday")
	}
	if IsServiceActive(db, "200", sunday) {
		t.Error("service 200 should NOT be active on Sunday")
	}

	// Service 180: only 2023-09-15, Friday.
	if !IsServiceActive(db, "180", friday) {
		t.Error("service 180 should be active on 2023-09-15")
	}
	if IsServiceActive(db, "180", saturday) {
		t.Error("service 180 should NOT be active on 2023-09-16 (outside date range)")
	}
}

// TestIsServiceActiveException verifies calendar_dates override.
func TestIsServiceActiveException(t *testing.T) {
	db := makeTestDB()
	friday := mustDate("2023-09-15")

	// Force service 200 OFF on this Friday via exception type 2.
	db.Exceptions["200:20230915"] = 2
	if IsServiceActive(db, "200", friday) {
		t.Error("service 200 should be forced OFF by exception type 2")
	}

	// Force service 999 (unknown) ON via exception type 1.
	db.Exceptions["999:20230915"] = 1
	if !IsServiceActive(db, "999", friday) {
		t.Error("unknown service 999 should be forced ON by exception type 1")
	}
}

// TestGetLiveDelay exercises the binary search in LiveStore.GetDelay.
func TestGetLiveDelay(t *testing.T) {
	ls := NewLiveStore()

	// Insert delays for trip "3582_6405": seqs 20, 50, 78, 100.
	ls.Delays["3582_6405"] = []StopDelay{
		{StopSequence: 20, DelaySeconds: 60},
		{StopSequence: 50, DelaySeconds: 30},
		{StopSequence: 78, DelaySeconds: 88},
		{StopSequence: 100, DelaySeconds: 120},
	}

	// Exact match at seq 78.
	sd, ok := ls.GetDelay("3582_6405", 78)
	if !ok {
		t.Fatal("expected delay for seq 78")
	}
	if sd.DelaySeconds != 88 {
		t.Errorf("seq 78: want delay 88, got %d", sd.DelaySeconds)
	}

	// Seq 60 → should fall back to seq 50 (the highest ≤ 60).
	sd, ok = ls.GetDelay("3582_6405", 60)
	if !ok {
		t.Fatal("expected delay for seq 60")
	}
	if sd.DelaySeconds != 30 {
		t.Errorf("seq 60 fallback: want 30, got %d", sd.DelaySeconds)
	}

	// Seq 5 → no lower sequence, should return not found.
	_, ok = ls.GetDelay("3582_6405", 5)
	if ok {
		t.Error("seq 5 should not find any delay (all seqs > 5)")
	}

	// Unknown trip.
	_, ok = ls.GetDelay("unknown_trip", 78)
	if ok {
		t.Error("unknown trip should return not found")
	}
}

// TestQueryArrivalsBasic checks that QueryArrivals returns arrivals in the right order.
func TestQueryArrivalsBasic(t *testing.T) {
	db := makeTestDB()
	ls := NewLiveStore()

	// "now" = 2023-09-15 09:10:00 UTC (matching Python test)
	now := time.Date(2023, 9, 15, 9, 10, 0, 0, time.UTC)

	arrivals := QueryArrivals(db, ls, "1358", now, 60, nil)
	if len(arrivals) == 0 {
		t.Fatal("expected at least one arrival")
	}

	// First arrival should be route 68 at 09:15:50.
	first := arrivals[0]
	if first.RouteShort != "68" {
		t.Errorf("first arrival: want route 68, got %q", first.RouteShort)
	}
	want := time.Date(2023, 9, 15, 9, 15, 50, 0, time.UTC)
	if !first.ScheduledTime.Equal(want) {
		t.Errorf("first arrival scheduled: want %v, got %v", want, first.ScheduledTime)
	}
	// No realtime data yet.
	if !first.RealtimeTime.IsZero() {
		t.Error("expected no realtime time without live data")
	}
}

// TestQueryArrivalsWithDelay verifies realtime delay is applied.
func TestQueryArrivalsWithDelay(t *testing.T) {
	db := makeTestDB()
	ls := NewLiveStore()

	// Add a −132 second delay for the 68 bus trip at stop 1358 (seq 45).
	ls.Delays["3582_9999"] = []StopDelay{
		{StopSequence: 45, DelaySeconds: -132},
	}

	now := time.Date(2023, 9, 15, 9, 10, 0, 0, time.UTC)
	arrivals := QueryArrivals(db, ls, "1358", now, 60, nil)

	var a68 *Arrival
	for i := range arrivals {
		if arrivals[i].RouteShort == "68" {
			a68 = &arrivals[i]
			break
		}
	}
	if a68 == nil {
		t.Fatal("68 arrival not found")
	}

	wantRT := time.Date(2023, 9, 15, 9, 13, 38, 0, time.UTC)
	if !a68.RealtimeTime.Equal(wantRT) {
		t.Errorf("68 realtime: want %v, got %v", wantRT, a68.RealtimeTime)
	}
	if a68.DelayMinutes != -2 {
		t.Errorf("68 delay: want -2 min, got %d", a68.DelayMinutes)
	}
}

// TestQueryArrivalsCancelled verifies cancelled trips are excluded.
func TestQueryArrivalsCancelled(t *testing.T) {
	db := makeTestDB()
	ls := NewLiveStore()
	ls.Cancellations["3582_9999"] = time.Now() // route 68 trip cancelled

	now := time.Date(2023, 9, 15, 9, 10, 0, 0, time.UTC)
	arrivals := QueryArrivals(db, ls, "1358", now, 60, nil)

	for _, a := range arrivals {
		if a.RouteShort == "68" {
			t.Error("cancelled 68 trip should not appear in arrivals")
		}
	}
}

// TestQueryArrivalsRouteFilter verifies route filtering works.
func TestQueryArrivalsRouteFilter(t *testing.T) {
	db := makeTestDB()
	ls := NewLiveStore()

	filter := BuildRouteFilter([]string{"49"})
	now := time.Date(2023, 9, 15, 9, 10, 0, 0, time.UTC)
	arrivals := QueryArrivals(db, ls, "1358", now, 60, filter)

	for _, a := range arrivals {
		if a.RouteShort != "49" {
			t.Errorf("route filter: expected only 49, got %q", a.RouteShort)
		}
	}
	if len(arrivals) == 0 {
		t.Error("expected at least one 49 arrival")
	}
}

// TestParseGTFSTime verifies overnight time parsing.
func TestParseGTFSTime(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"09:15:50", 9*3600 + 15*60 + 50},
		{"00:00:00", 0},
		{"26:05:00", 26*3600 + 5*60},
		{"23:59:59", 23*3600 + 59*60 + 59},
	}
	for _, c := range cases {
		got, err := parseGTFSTime(c.input)
		if err != nil {
			t.Errorf("parseGTFSTime(%q): %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseGTFSTime(%q): want %d, got %d", c.input, c.want, got)
		}
	}
}
