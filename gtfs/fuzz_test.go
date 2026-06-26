package gtfs

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzFuzzBench_ParseGTFSTime verifies that parseGTFSTime never panics on
// arbitrary string input. Seeds cover valid GTFS times and various malformed
// inputs. FR8.
func FuzzFuzzBench_ParseGTFSTime(f *testing.F) {
	// Valid GTFS time strings (including overnight >23h).
	f.Add("09:15:50")
	f.Add("00:00:00")
	f.Add("26:05:00")
	f.Add("23:59:59")
	// Malformed inputs.
	f.Add("")
	f.Add("not:a:time")
	f.Add("99:99:99")
	f.Add(":::")
	f.Add("1:2")
	f.Add("-1:00:00")

	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseGTFSTime(s) // must not panic
	})
}

// FuzzFuzzBench_BuildFromZIPFile verifies that BuildFromZIPFile never panics
// on arbitrary bytes. The fuzz bytes are written to a temp file and passed as
// the ZIP path. Must return either a *StaticDB or an error — never panic. FR8.
func FuzzFuzzBench_BuildFromZIPFile(f *testing.F) {
	// Seed with the real trimmed GTFS static fixture.
	validZip, err := os.ReadFile("testdata/gtfs_static.zip")
	if err != nil {
		f.Fatalf("reading fixture zip: %v", err)
	}
	f.Add(validZip)
	// Invalid / edge-case seeds.
	f.Add([]byte{})
	f.Add([]byte("not a zip file at all"))
	f.Add([]byte("PK\x03\x04")) // ZIP magic bytes but truncated body

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.zip")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Skip(err)
		}
		_, _ = BuildFromZIPFile(path, []string{"478", "2808"}) // must not panic
	})
}

// FuzzFuzzBench_PollerParse verifies that Poller.parse never panics on
// arbitrary bytes (valid and invalid GTFS-RT protobufs alike). FR8.
func FuzzFuzzBench_PollerParse(f *testing.F) {
	// Seed with the real captured realtime feed.
	validFeed, err := os.ReadFile("testdata/tripupdates.gtfsr")
	if err != nil {
		f.Fatalf("reading fixture feed: %v", err)
	}
	f.Add(validFeed)
	// Invalid / minimal seeds.
	f.Add([]byte{})
	f.Add([]byte("not protobuf"))
	f.Add([]byte("\x0a\x00")) // minimal proto envelope (field 1, length-delimited, empty)

	// Shared read-only StaticDB; parse only reads it via p.db.Load().
	fuzzBenchPollerDB := makeTestDB()

	f.Fuzz(func(t *testing.T, data []byte) {
		poller := NewPoller("", "", NewDB(fuzzBenchPollerDB))
		withClock(poller.Store(), fixtureNow())
		_ = poller.parse(data) // must not panic; error is acceptable
	})
}
