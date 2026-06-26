package gtfs

import (
	"os"
	"testing"
	"time"
)

// This file is the shared scaffolding for fixture-based tests in package gtfs.
// All fixture-driven tests should reuse these helpers (and makeTestDB/mustDate/
// addStopTime in arrivals_test.go) rather than redefining their own, so test
// files stay independent and merge cleanly.

// fixtureFeedUnix is the GTFS-Realtime feed header timestamp of the canonical
// capture (testdata/tripupdates.gtfsr). Every fixture-based test pins `now` to
// this instant so captured delays/ETAs map to fixed minutes-until values.
// See testdata/README.md.
const fixtureFeedUnix int64 = 1782497185

// fixtureStops are the stop_codes the committed fixtures were trimmed to.
func fixtureStops() []string { return []string{"478", "2808", "999126"} }

// fixtureNow returns the canonical test instant located in Europe/Dublin — the
// same wall-clock zone the Pi runs in production. The Location matters:
// QueryArrivals reconstructs scheduled times relative to midnight in
// now.Location(), so using Dublin (not UTC) makes schedule reconstruction and
// absolute-timestamp delay maths match what the device computes. Falls back to
// UTC only if the zone database is unavailable.
func fixtureNow() time.Time { return fixtureNowIn("Europe/Dublin") }

// fixtureNowIn returns the canonical instant in the named location (falls back to
// UTC if the zone can't be loaded).
func fixtureNowIn(name string) time.Time {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Unix(fixtureFeedUnix, 0).UTC()
	}
	return time.Unix(fixtureFeedUnix, 0).In(loc)
}

// loadFixtureDB parses the trimmed static GTFS fixture into a StaticDB.
func loadFixtureDB(t *testing.T) *StaticDB {
	t.Helper()
	db, err := BuildFromZIPFile("testdata/gtfs_static.zip", fixtureStops())
	if err != nil {
		t.Fatalf("loading static fixture: %v", err)
	}
	return db
}

// loadFixtureFeed returns the raw bytes of the canonical realtime capture.
func loadFixtureFeed(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/tripupdates.gtfsr")
	if err != nil {
		t.Fatalf("loading realtime fixture: %v", err)
	}
	return data
}

// withClock pins a LiveStore's clock to a fixed instant, making the 24h
// cancellation window and poll timestamps deterministic. Call before any
// concurrent use — `now` is set once and must not be reassigned while other
// goroutines read it.
func withClock(ls *LiveStore, now time.Time) *LiveStore {
	ls.now = func() time.Time { return now }
	return ls
}
