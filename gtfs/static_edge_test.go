package gtfs

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// FR2 — static parse/cache/refresh pipeline tests.
//
// All helpers are prefixed "staticPipe" to avoid collisions with the other
// hardening files in this package.  Tests are named TestStaticPipe_<Case>.
//
// Build strategy: minimal synthetic GTFS ZIPs are assembled in-code with
// archive/zip so every edge case can be exercised without touching real
// network calls or real system paths. httptest.Server provides a controllable
// Last-Modified header for LoadOrBuild / MaybeRebuild / isNewerZIPAvailable.
// loadFixtureDB is used where a realistic payload is beneficial (gob round-trip).

// ---- shared helpers ----

// staticPipeZIPBytes builds a GTFS ZIP from a file-name→CSV-content map and
// returns the raw bytes.
func staticPipeZIPBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatalf("staticPipeZIPBytes: create entry %q: %v", name, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("staticPipeZIPBytes: write entry %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("staticPipeZIPBytes: close: %v", err)
	}
	return buf.Bytes()
}

// staticPipeZIPFile writes a GTFS ZIP into dir and returns the path.
func staticPipeZIPFile(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "gtfs_test.zip")
	if err := os.WriteFile(path, staticPipeZIPBytes(t, files), 0o644); err != nil {
		t.Fatalf("staticPipeZIPFile: %v", err)
	}
	return path
}

// staticPipeBaseFiles returns a minimal set of valid GTFS CSVs for a single
// stop, route, service and trip.  Callers may override individual entries.
// stopID and stopCode may differ to exercise the StopIDToNumber path.
func staticPipeBaseFiles(stopID, stopCode, routeID, tripID, serviceID string) map[string]string {
	return map[string]string{
		"stops.txt": "stop_id,stop_code,stop_name\n" +
			stopID + "," + stopCode + ",Test Stop\n",
		"routes.txt": "route_id,route_short_name\n" +
			routeID + ",R1\n",
		"trips.txt": "trip_id,route_id,service_id,trip_headsign\n" +
			tripID + "," + routeID + "," + serviceID + ",Test Dest\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
			tripID + ",10:00:00,10:00:00," + stopID + ",1\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			serviceID + ",1,1,1,1,1,0,0,20230101,20231231\n",
		"calendar_dates.txt": "service_id,date,exception_type\n",
	}
}

// staticPipeServer creates an httptest.Server that returns the given ZIP bytes
// with the given Last-Modified on both HEAD and GET requests.  A request counter
// (incremented on every hit) is returned so callers can assert call counts.
func staticPipeServer(t *testing.T, zipData []byte, lastMod time.Time) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Last-Modified", lastMod.UTC().Format(http.TimeFormat))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zipData)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// staticPipeWriteGob saves db to cachePath via the package-private saveGob.
func staticPipeWriteGob(t *testing.T, cachePath string, db *StaticDB) {
	t.Helper()
	if err := saveGob(cachePath, db); err != nil {
		t.Fatalf("staticPipeWriteGob: %v", err)
	}
}

// staticPipeValidCache builds a StaticDB from a ZIP file, persists it as a gob
// in dir/static_cache.gob and returns (db, cachePath).
func staticPipeValidCache(t *testing.T, dir string, files map[string]string, filterStops []string) (*StaticDB, string) {
	t.Helper()
	zipPath := staticPipeZIPFile(t, dir, files)
	db, err := BuildFromZIPFile(zipPath, filterStops)
	if err != nil {
		t.Fatalf("staticPipeValidCache: BuildFromZIPFile: %v", err)
	}
	cachePath := filepath.Join(dir, "static_cache.gob")
	staticPipeWriteGob(t, cachePath, db)
	return db, cachePath
}

// ---- BOM stripping ----

// TestStaticPipe_BOMStripped verifies that a UTF-8 BOM on the first header field
// of any CSV is silently removed so the column name is resolved correctly.
func TestStaticPipe_BOMStripped(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("1001", "1001", "R1", "T1", "S1")
	// Prepend BOM to the stop_id header so parseCSV must strip it.
	files["stops.txt"] = "\xef\xbb\xbfstop_id,stop_code,stop_name\n1001,1001,BOM Stop\n"
	path := staticPipeZIPFile(t, dir, files)

	db, err := BuildFromZIPFile(path, []string{"1001"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile: %v", err)
	}
	if _, ok := db.StopNames["1001"]; !ok {
		t.Error("BOM prefix on stop_id column should be stripped; stop not found in StopNames")
	}
	if len(db.StopTimes["1001"]) == 0 {
		t.Error("stop_times for stop 1001 should be populated after BOM stripping")
	}
}

// ---- stop_code fallback ----

// TestStaticPipe_StopCodeFallback verifies that an empty or "0" stop_code causes
// the stop_id to be used as the stop number.
func TestStaticPipe_StopCodeFallback(t *testing.T) {
	t.Run("empty_stop_code", func(t *testing.T) {
		dir := t.TempDir()
		// stop_code field is present but empty.
		files := staticPipeBaseFiles("2001", "", "R1", "T1", "S1")
		path := staticPipeZIPFile(t, dir, files)

		db, err := BuildFromZIPFile(path, []string{"2001"})
		if err != nil {
			t.Fatalf("BuildFromZIPFile: %v", err)
		}
		if _, ok := db.StopNames["2001"]; !ok {
			t.Error("empty stop_code: stop should be keyed by stop_id (2001)")
		}
	})

	t.Run("zero_stop_code", func(t *testing.T) {
		dir := t.TempDir()
		// stop_code is the literal string "0".
		files := staticPipeBaseFiles("2002", "0", "R1", "T2", "S1")
		path := staticPipeZIPFile(t, dir, files)

		db, err := BuildFromZIPFile(path, []string{"2002"})
		if err != nil {
			t.Fatalf("BuildFromZIPFile: %v", err)
		}
		if _, ok := db.StopNames["2002"]; !ok {
			t.Errorf(`stop_code "0": stop should be keyed by stop_id (2002)`)
		}
	})
}

// ---- platform_code ----

// TestStaticPipe_PlatformCode verifies StopPlatforms is populated when
// platform_code is present and non-empty, and is absent in all other cases.
func TestStaticPipe_PlatformCode(t *testing.T) {
	t.Run("present_non_empty", func(t *testing.T) {
		dir := t.TempDir()
		files := staticPipeBaseFiles("3001", "3001", "R1", "T1", "S1")
		files["stops.txt"] = "stop_id,stop_code,stop_name,platform_code\n3001,3001,Platform Stop,7\n"
		path := staticPipeZIPFile(t, dir, files)

		db, err := BuildFromZIPFile(path, []string{"3001"})
		if err != nil {
			t.Fatalf("BuildFromZIPFile: %v", err)
		}
		if db.StopPlatforms["3001"] != "7" {
			t.Errorf("StopPlatforms[3001]: want %q, got %q", "7", db.StopPlatforms["3001"])
		}
	})

	t.Run("column_absent", func(t *testing.T) {
		dir := t.TempDir()
		// No platform_code column at all in stops.txt.
		files := staticPipeBaseFiles("3002", "3002", "R1", "T2", "S1")
		path := staticPipeZIPFile(t, dir, files)

		db, err := BuildFromZIPFile(path, []string{"3002"})
		if err != nil {
			t.Fatalf("BuildFromZIPFile: %v", err)
		}
		if p := db.StopPlatforms["3002"]; p != "" {
			t.Errorf("StopPlatforms should be absent when column is missing, got %q", p)
		}
	})

	t.Run("column_present_empty_value", func(t *testing.T) {
		dir := t.TempDir()
		// platform_code column exists but the value is empty.
		files := staticPipeBaseFiles("3003", "3003", "R1", "T3", "S1")
		files["stops.txt"] = "stop_id,stop_code,stop_name,platform_code\n3003,3003,No Plat,\n"
		path := staticPipeZIPFile(t, dir, files)

		db, err := BuildFromZIPFile(path, []string{"3003"})
		if err != nil {
			t.Fatalf("BuildFromZIPFile: %v", err)
		}
		if p := db.StopPlatforms["3003"]; p != "" {
			t.Errorf("empty platform_code value should leave StopPlatforms absent, got %q", p)
		}
	})
}

// ---- StopIDToNumber ----

// TestStaticPipe_StopIDToNumber verifies the map is populated exactly for
// configured stops where stop_id differs from stop_code.
func TestStaticPipe_StopIDToNumber(t *testing.T) {
	dir := t.TempDir()
	// stop_id "RAIL_4001" maps to stop_code "4001" — they differ and "4001" is
	// in filterStops.
	files := staticPipeBaseFiles("RAIL_4001", "4001", "R1", "T1", "S1")
	// Add a second stop where stop_id == stop_code → must NOT appear in the map.
	files["stops.txt"] += "4002,4002,Bus Stop\n"
	path := staticPipeZIPFile(t, dir, files)

	db, err := BuildFromZIPFile(path, []string{"4001", "4002"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile: %v", err)
	}

	if got := db.StopIDToNumber["RAIL_4001"]; got != "4001" {
		t.Errorf("StopIDToNumber[RAIL_4001]: want %q, got %q", "4001", got)
	}
	if _, ok := db.StopIDToNumber["4002"]; ok {
		t.Error("StopIDToNumber should not contain an entry where stop_id == stop_code")
	}
}

// TestStaticPipe_StopIDToNumber_NotInFilter verifies that a stop whose stop_code
// is NOT in filterStops is excluded from StopIDToNumber even if stop_id ≠ stop_code.
func TestStaticPipe_StopIDToNumber_NotInFilter(t *testing.T) {
	dir := t.TempDir()
	// "RAIL_4099" → "4099", but "4099" is not in filterStops.
	files := staticPipeBaseFiles("RAIL_4099", "4099", "R1", "T1", "S1")
	path := staticPipeZIPFile(t, dir, files)

	db, err := BuildFromZIPFile(path, []string{"OTHER"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile: %v", err)
	}
	if _, ok := db.StopIDToNumber["RAIL_4099"]; ok {
		t.Error("StopIDToNumber must not contain stops outside the configured filter set")
	}
}

// ---- trip filtering ----

// TestStaticPipe_TripFiltering verifies that only trips with a stop_time at one
// of the configured stops are included in db.Trips.
func TestStaticPipe_TripFiltering(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"stops.txt": "stop_id,stop_code,stop_name\n" +
			"5001,5001,Kept Stop\n" +
			"5002,5002,Dropped Stop\n",
		"routes.txt": "route_id,route_short_name\nR1,Bus1\n",
		"trips.txt": "trip_id,route_id,service_id,trip_headsign\n" +
			"KEEP_TRIP,R1,S1,Kept Dest\n" +
			"DROP_TRIP,R1,S1,Dropped Dest\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
			"KEEP_TRIP,10:00:00,10:00:00,5001,1\n" +
			"DROP_TRIP,10:30:00,10:30:00,5002,1\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			"S1,1,1,1,1,1,0,0,20230101,20231231\n",
		"calendar_dates.txt": "service_id,date,exception_type\n",
	}
	path := staticPipeZIPFile(t, dir, files)

	// Only stop 5001 is configured; DROP_TRIP serves only 5002.
	db, err := BuildFromZIPFile(path, []string{"5001"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile: %v", err)
	}

	if _, ok := db.Trips["KEEP_TRIP"]; !ok {
		t.Error("KEEP_TRIP serves a configured stop and must be in db.Trips")
	}
	if _, ok := db.Trips["DROP_TRIP"]; ok {
		t.Error("DROP_TRIP serves no configured stop and must be excluded from db.Trips")
	}
}

// ---- missing calendar.txt ----

// TestStaticPipe_MissingCalendarTolerated verifies no error when calendar.txt is
// absent from the ZIP (some feeds use only calendar_dates.txt).
func TestStaticPipe_MissingCalendarTolerated(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("6001", "6001", "R1", "T1", "S1")
	delete(files, "calendar.txt")
	path := staticPipeZIPFile(t, dir, files)

	db, err := BuildFromZIPFile(path, []string{"6001"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile should tolerate absent calendar.txt: %v", err)
	}
	if db.Services == nil {
		t.Error("db.Services must be initialised (non-nil) even without calendar.txt")
	}
	// calendar_dates.txt is still present so Exceptions may be populated; the
	// important assertion is just that we don't crash.
}

// ---- malformed stop_times rows ----

// TestStaticPipe_MalformedStopTimeSkipped verifies that a stop_times row with an
// unparseable arrival_time is silently skipped; valid rows after it still load.
func TestStaticPipe_MalformedStopTimeSkipped(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("7001", "7001", "R1", "T1", "S1")
	// Two rows: first has a bad arrival_time, second is valid (10:30 = bucket 10).
	files["stop_times.txt"] = "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
		"T1,NOT_A_TIME,10:00:00,7001,1\n" +
		"T1,10:30:00,10:30:00,7001,2\n"
	path := staticPipeZIPFile(t, dir, files)

	db, err := BuildFromZIPFile(path, []string{"7001"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile with malformed row: %v", err)
	}
	times := db.StopTimes["7001"]
	if times == nil || len(times[10]) == 0 {
		t.Error("valid stop_time at 10:30 (bucket 10) must survive a preceding malformed row")
	}
}

// ---- overnight hour bucketing ----

// TestStaticPipe_OvernightBucket verifies that arrival time 25:30:00 is filed
// into hour bucket 1 (25 mod 24) and that the raw ArrivalSecs value is preserved.
func TestStaticPipe_OvernightBucket(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("8001", "8001", "R1", "T1", "S1")
	files["stop_times.txt"] = "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
		"T1,25:30:00,25:30:00,8001,1\n"
	path := staticPipeZIPFile(t, dir, files)

	db, err := BuildFromZIPFile(path, []string{"8001"})
	if err != nil {
		t.Fatalf("BuildFromZIPFile: %v", err)
	}

	// 25*3600 + 30*60 = 91800 s; (91800/3600)%24 = 25%24 = 1
	const wantBucket = 1
	times := db.StopTimes["8001"]
	if times == nil {
		t.Fatal("no stop_times for stop 8001")
	}
	if len(times[wantBucket]) == 0 {
		t.Errorf("25:30:00 should be in hour bucket %d (25%%24=%d)", wantBucket, wantBucket)
	}
	if len(times[25]) > 0 {
		t.Error("25:30:00 must NOT be stored in raw bucket 25 — only buckets 0-23 are valid")
	}
	// ArrivalSecs is the raw value, not wrapped mod 86400.
	const wantSecs = 25*3600 + 30*60
	if st := times[wantBucket][0]; st.ArrivalSecs != wantSecs {
		t.Errorf("ArrivalSecs: want %d (25:30:00), got %d", wantSecs, st.ArrivalSecs)
	}
}

// ---- parseGTFSDate ----

// TestStaticPipe_ParseGTFSDate covers the plain YYYYMMDD form, the hyphenated
// YYYY-MM-DD form (hyphens stripped), and invalid inputs.
func TestStaticPipe_ParseGTFSDate(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
		year    int
		month   time.Month
		day     int
	}{
		{"20231015", false, 2023, time.October, 15},
		{"2023-10-15", false, 2023, time.October, 15}, // hyphens stripped
		{"20000101", false, 2000, time.January, 1},
		{"20231232", true, 0, 0, 0}, // day 32 is invalid
		{"", true, 0, 0, 0},         // empty string
		{"2023101", true, 0, 0, 0},  // too short (7 digits)
	}
	for _, c := range cases {
		t.Run("input="+c.input, func(t *testing.T) {
			got, err := parseGTFSDate(c.input)
			if c.wantErr {
				if err == nil {
					t.Errorf("parseGTFSDate(%q): expected error, got %v", c.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGTFSDate(%q): unexpected error: %v", c.input, err)
			}
			if got.Year() != c.year || got.Month() != c.month || got.Day() != c.day {
				t.Errorf("parseGTFSDate(%q): want %d-%02d-%02d, got %v",
					c.input, c.year, int(c.month), c.day, got)
			}
		})
	}
}

// ---- parseGTFSTime edge cases ----

// TestStaticPipe_ParseGTFSTimeEdge extends the existing TestParseGTFSTime in
// arrivals_test.go with error paths and additional overnight boundary cases.
func TestStaticPipe_ParseGTFSTimeEdge(t *testing.T) {
	// Error cases — parseGTFSTime must return a non-nil error for all of these.
	errCases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"two_parts", "10:05"},
		{"four_parts", "10:00:00:00"},
		{"non_numeric_hour", "aa:00:00"},
		{"non_numeric_minute", "10:bb:00"},
		{"non_numeric_second", "10:00:cc"},
	}
	for _, c := range errCases {
		t.Run("error/"+c.name, func(t *testing.T) {
			_, err := parseGTFSTime(c.input)
			if err == nil {
				t.Errorf("parseGTFSTime(%q): expected error, got none", c.input)
			}
		})
	}

	// Valid cases that are NOT already covered by TestParseGTFSTime.
	okCases := []struct {
		input    string
		wantSecs int
	}{
		// 24:00:00 — legal GTFS "end of service day" value.
		{"24:00:00", 24 * 3600},
		// 25:30:00 — overnight trip arriving the next calendar day.
		{"25:30:00", 25*3600 + 30*60},
		// 28:00:00 — extreme overnight (4 am next day).
		{"28:00:00", 28 * 3600},
		// Single-digit hour/minute/second with leading zero.
		{"01:02:03", 1*3600 + 2*60 + 3},
	}
	for _, c := range okCases {
		t.Run("ok/"+c.input, func(t *testing.T) {
			got, err := parseGTFSTime(c.input)
			if err != nil {
				t.Fatalf("parseGTFSTime(%q): unexpected error: %v", c.input, err)
			}
			if got != c.wantSecs {
				t.Errorf("parseGTFSTime(%q): want %d, got %d", c.input, c.wantSecs, got)
			}
		})
	}
}

// ---- gob round-trip ----

// TestStaticPipe_GobRoundTrip verifies that saveGob followed by loadGob
// reproduces a StaticDB faithfully. Uses loadFixtureDB for a realistic payload.
func TestStaticPipe_GobRoundTrip(t *testing.T) {
	original := loadFixtureDB(t)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "roundtrip.gob")

	if err := saveGob(cachePath, original); err != nil {
		t.Fatalf("saveGob: %v", err)
	}

	loaded, err := loadGob(cachePath)
	if err != nil {
		t.Fatalf("loadGob: %v", err)
	}

	if loaded.SchemaVer != original.SchemaVer {
		t.Errorf("SchemaVer: want %d, got %d", original.SchemaVer, loaded.SchemaVer)
	}
	if len(loaded.Trips) != len(original.Trips) {
		t.Errorf("Trips count: want %d, got %d", len(original.Trips), len(loaded.Trips))
	}
	if len(loaded.Services) != len(original.Services) {
		t.Errorf("Services count: want %d, got %d", len(original.Services), len(loaded.Services))
	}
	if len(loaded.StopTimes) != len(original.StopTimes) {
		t.Errorf("StopTimes stop count: want %d, got %d", len(original.StopTimes), len(loaded.StopTimes))
	}
	if len(loaded.Exceptions) != len(original.Exceptions) {
		t.Errorf("Exceptions count: want %d, got %d", len(original.Exceptions), len(loaded.Exceptions))
	}
	if !loaded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp: want %v, got %v", original.Timestamp, loaded.Timestamp)
	}
	if !slicesEqual(loaded.FilterStops, original.FilterStops) {
		t.Errorf("FilterStops: want %v, got %v", original.FilterStops, loaded.FilterStops)
	}

	// Spot-check a known fixture stop.
	const knownStop = "478"
	if len(loaded.StopTimes[knownStop]) == 0 {
		t.Errorf("stop %s should have hour-bucketed stop_times after gob round-trip", knownStop)
	}
	if loaded.StopNames[knownStop] == "" {
		t.Errorf("stop %s should have a name after gob round-trip", knownStop)
	}
}

// TestStaticPipe_GobLoadMissing verifies loadGob returns an error for a path
// that does not exist (no crash, no silent zero value).
func TestStaticPipe_GobLoadMissing(t *testing.T) {
	_, err := loadGob(filepath.Join(t.TempDir(), "does_not_exist.gob"))
	if err == nil {
		t.Error("loadGob on a missing file must return a non-nil error")
	}
}

// TestStaticPipe_GobLoadCorrupt verifies loadGob returns an error for a file
// that is not a valid gob stream.
func TestStaticPipe_GobLoadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.gob")
	if err := os.WriteFile(path, []byte("this is not a valid gob payload"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	_, err := loadGob(path)
	if err == nil {
		t.Error("loadGob on a corrupt file must return a non-nil error")
	}
}

// ---- LoadOrBuild ----

// TestStaticPipe_LoadOrBuildCacheReuse verifies that LoadOrBuild returns the
// cached DB (only one HEAD request; no GET) when the cache is valid and the
// server's Last-Modified is not newer than the cached Timestamp.
func TestStaticPipe_LoadOrBuildCacheReuse(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9001", "9001", "R1", "T1", "S1")
	filterStops := []string{"9001"}

	// Build and persist a warm cache.
	origDB, _ := staticPipeValidCache(t, dir, files, filterStops)

	// Server reports an older Last-Modified than the cached Timestamp.
	olderTime := origDB.Timestamp.Add(-1 * time.Hour)
	srv, reqCount := staticPipeServer(t, staticPipeZIPBytes(t, files), olderTime)

	got, err := LoadOrBuild(srv.URL, dir, filterStops)
	if err != nil {
		t.Fatalf("LoadOrBuild (cache reuse): %v", err)
	}
	if got == nil {
		t.Fatal("LoadOrBuild returned nil with a valid cache")
	}
	// Exactly one HEAD request for the freshness check; no GET (no download).
	if n := reqCount.Load(); n != 1 {
		t.Errorf("cache-reuse path: want 1 HEAD request, got %d", n)
	}
}

// TestStaticPipe_LoadOrBuildSchemaMismatch verifies that LoadOrBuild discards a
// cached gob whose SchemaVer is stale and falls through to a full rebuild (GET).
func TestStaticPipe_LoadOrBuildSchemaMismatch(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9101", "9101", "R1", "T1", "S1")
	filterStops := []string{"9101"}

	// Write a gob whose SchemaVer is one behind the current version.
	staleDB := &StaticDB{
		StopTimes:       make(map[string]map[int][]StopTime),
		Trips:           make(map[string]Trip),
		Services:        make(map[string]Service),
		Exceptions:      make(map[string]int),
		StopNames:       make(map[string]string),
		StopPlatforms:   make(map[string]string),
		RouteShortNames: make(map[string]string),
		StopIDToNumber:  make(map[string]string),
		FilterStops:     filterStops,
		SchemaVer:       schemaVer - 1,
	}
	cachePath := filepath.Join(dir, "static_cache.gob")
	staticPipeWriteGob(t, cachePath, staleDB)

	srv, reqCount := staticPipeServer(t, staticPipeZIPBytes(t, files), time.Now().Add(24*time.Hour))

	got, err := LoadOrBuild(srv.URL, dir, filterStops)
	if err != nil {
		t.Fatalf("LoadOrBuild (schema mismatch): %v", err)
	}
	if got == nil {
		t.Fatal("LoadOrBuild returned nil after schema mismatch rebuild")
	}
	// Schema mismatch skips the HEAD check and goes straight to a GET rebuild.
	if n := reqCount.Load(); n < 1 {
		t.Errorf("schema mismatch must trigger a rebuild (at least 1 GET); got %d requests", n)
	}
	if got.SchemaVer != schemaVer {
		t.Errorf("rebuilt DB SchemaVer: want %d, got %d", schemaVer, got.SchemaVer)
	}
}

// TestStaticPipe_LoadOrBuildFilterChange verifies that LoadOrBuild discards a
// cached gob whose FilterStops differ from the requested set and rebuilds.
func TestStaticPipe_LoadOrBuildFilterChange(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"stops.txt": "stop_id,stop_code,stop_name\n9201,9201,Stop A\n9202,9202,Stop B\n",
		"routes.txt": "route_id,route_short_name\nR1,R1\n",
		"trips.txt": "trip_id,route_id,service_id,trip_headsign\nT1,R1,S1,Dest\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
			"T1,10:00:00,10:00:00,9201,1\n" +
			"T1,10:15:00,10:15:00,9202,2\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			"S1,1,1,1,1,1,0,0,20230101,20231231\n",
		"calendar_dates.txt": "service_id,date,exception_type\n",
	}

	// Warm cache for only stop 9201.
	staticPipeValidCache(t, dir, files, []string{"9201"})

	srv, reqCount := staticPipeServer(t, staticPipeZIPBytes(t, files), time.Now().Add(24*time.Hour))

	// Request both stops — FilterStops mismatch should trigger a rebuild.
	newFilter := []string{"9201", "9202"}
	got, err := LoadOrBuild(srv.URL, dir, newFilter)
	if err != nil {
		t.Fatalf("LoadOrBuild (filter change): %v", err)
	}
	if got == nil {
		t.Fatal("LoadOrBuild returned nil after filter change rebuild")
	}
	if n := reqCount.Load(); n < 1 {
		t.Errorf("filter change must trigger a rebuild (at least 1 GET); got %d requests", n)
	}
	// The rebuilt DB should now include stop 9202.
	if len(got.StopTimes["9202"]) == 0 {
		t.Error("rebuilt DB after filter change should include stop_times for new stop 9202")
	}
}

// TestStaticPipe_LoadOrBuildNewerZIP verifies that LoadOrBuild rebuilds when
// the cache is otherwise valid but the server reports a newer Last-Modified.
func TestStaticPipe_LoadOrBuildNewerZIP(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9301", "9301", "R1", "T1", "S1")
	filterStops := []string{"9301"}

	origDB, _ := staticPipeValidCache(t, dir, files, filterStops)

	// Server reports a time strictly after the cached Timestamp.
	newerTime := origDB.Timestamp.Add(2 * time.Hour)
	srv, reqCount := staticPipeServer(t, staticPipeZIPBytes(t, files), newerTime)

	got, err := LoadOrBuild(srv.URL, dir, filterStops)
	if err != nil {
		t.Fatalf("LoadOrBuild (newer ZIP): %v", err)
	}
	if got == nil {
		t.Fatal("LoadOrBuild returned nil after newer-ZIP rebuild")
	}
	// HEAD check fires first, then a GET for the rebuild: at least 2 requests.
	if n := reqCount.Load(); n < 2 {
		t.Errorf("newer ZIP: want HEAD+GET (≥2 requests), got %d", n)
	}
}

// ---- MaybeRebuild / isNewerZIPAvailable ----

// TestStaticPipe_MaybeRebuildNewerAvailable verifies that MaybeRebuild returns a
// freshly built DB when the server's Last-Modified is newer than currentTimestamp.
func TestStaticPipe_MaybeRebuildNewerAvailable(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9401", "9401", "R1", "T1", "S1")
	filterStops := []string{"9401"}

	currentTS := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	newerTS := currentTS.Add(24 * time.Hour)

	srv, _ := staticPipeServer(t, staticPipeZIPBytes(t, files), newerTS)

	db, err := MaybeRebuild(srv.URL, dir, filterStops, currentTS)
	if err != nil {
		t.Fatalf("MaybeRebuild (newer): %v", err)
	}
	if db == nil {
		t.Fatal("MaybeRebuild should return a new DB when a newer ZIP is available")
	}
	if len(db.StopTimes) == 0 {
		t.Error("rebuilt DB should contain stop_times")
	}
}

// TestStaticPipe_MaybeRebuildNothingToDo verifies that MaybeRebuild returns
// (nil, nil) when the server's Last-Modified equals currentTimestamp (not newer).
func TestStaticPipe_MaybeRebuildNothingToDo(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9501", "9501", "R1", "T1", "S1")
	filterStops := []string{"9501"}

	currentTS := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	// Server reports the same time — not strictly newer.
	srv, _ := staticPipeServer(t, staticPipeZIPBytes(t, files), currentTS)

	db, err := MaybeRebuild(srv.URL, dir, filterStops, currentTS)
	if err != nil {
		t.Fatalf("MaybeRebuild (same timestamp): %v", err)
	}
	if db != nil {
		t.Error("MaybeRebuild should return nil when no newer ZIP is available")
	}
}

// TestStaticPipe_MaybeRebuildOlderZIP verifies the strictly-older case also
// returns (nil, nil).
func TestStaticPipe_MaybeRebuildOlderZIP(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9601", "9601", "R1", "T1", "S1")
	filterStops := []string{"9601"}

	currentTS := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	olderTS := currentTS.Add(-1 * time.Hour)
	srv, _ := staticPipeServer(t, staticPipeZIPBytes(t, files), olderTS)

	db, err := MaybeRebuild(srv.URL, dir, filterStops, currentTS)
	if err != nil {
		t.Fatalf("MaybeRebuild (older): %v", err)
	}
	if db != nil {
		t.Error("MaybeRebuild should return nil when server ZIP is older than current")
	}
}

// TestStaticPipe_MaybeRebuildPersistsGob verifies that a successful MaybeRebuild
// writes the gob cache to disk so a subsequent LoadOrBuild can use it.
func TestStaticPipe_MaybeRebuildPersistsGob(t *testing.T) {
	dir := t.TempDir()
	files := staticPipeBaseFiles("9701", "9701", "R1", "T1", "S1")
	filterStops := []string{"9701"}

	currentTS := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	newerTS := currentTS.Add(48 * time.Hour)

	srv, _ := staticPipeServer(t, staticPipeZIPBytes(t, files), newerTS)

	if _, err := MaybeRebuild(srv.URL, dir, filterStops, currentTS); err != nil {
		t.Fatalf("MaybeRebuild: %v", err)
	}

	cachePath := filepath.Join(dir, "static_cache.gob")
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		t.Fatal("MaybeRebuild should have written the gob cache to disk")
	}

	loaded, err := loadGob(cachePath)
	if err != nil {
		t.Fatalf("loadGob after MaybeRebuild: %v", err)
	}
	if len(loaded.StopTimes) == 0 {
		t.Error("gob written by MaybeRebuild should contain stop_times")
	}
}

// TestStaticPipe_MaybeRebuildNoLastModified verifies that a server returning no
// Last-Modified header causes MaybeRebuild to treat the ZIP as not newer and
// return (nil, nil).
func TestStaticPipe_MaybeRebuildNoLastModified(t *testing.T) {
	dir := t.TempDir()
	filterStops := []string{"9801"}

	// Server sets no Last-Modified header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	currentTS := time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC)
	db, err := MaybeRebuild(srv.URL, dir, filterStops, currentTS)
	if err != nil {
		t.Fatalf("MaybeRebuild (no Last-Modified): %v", err)
	}
	if db != nil {
		t.Error("absent Last-Modified should be treated as not-newer; MaybeRebuild must return nil")
	}
}

// TestStaticPipe_LoadOrBuildFromFixture exercises LoadOrBuild end-to-end using
// the committed real fixture ZIP served by an httptest.Server. The second call
// should be served from cache (only 1 HEAD, no GET).
func TestStaticPipe_LoadOrBuildFromFixture(t *testing.T) {
	zipData, err := os.ReadFile("testdata/gtfs_static.zip")
	if err != nil {
		t.Fatalf("read fixture ZIP: %v", err)
	}

	// A zipTime in the past so the gob cache built on the first call is
	// considered fresh (not newer) on the second call's HEAD check.
	zipTime := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	srv, reqCount := staticPipeServer(t, zipData, zipTime)

	dir := t.TempDir()
	stops := fixtureStops()

	// First call: no cache → full download + parse.
	db1, err := LoadOrBuild(srv.URL, dir, stops)
	if err != nil {
		t.Fatalf("LoadOrBuild (first call): %v", err)
	}
	if db1 == nil {
		t.Fatal("LoadOrBuild returned nil on first call")
	}
	for _, s := range stops {
		if len(db1.StopTimes[s]) == 0 {
			t.Errorf("stop %s: no stop_times after LoadOrBuild", s)
		}
	}

	afterFirst := reqCount.Load()

	// Second call: cache is valid and server is not newer → should return from cache.
	db2, err := LoadOrBuild(srv.URL, dir, stops)
	if err != nil {
		t.Fatalf("LoadOrBuild (second call): %v", err)
	}
	if db2 == nil {
		t.Fatal("LoadOrBuild returned nil on second call")
	}

	// Second call should add exactly 1 request (the HEAD freshness check).
	if extra := reqCount.Load() - afterFirst; extra != 1 {
		t.Errorf("second LoadOrBuild should make exactly 1 HEAD request; made %d extra", extra)
	}
}
