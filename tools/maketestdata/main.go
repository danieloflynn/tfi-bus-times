// Command maketestdata trims the full TFI GTFS static ZIP down to a small fixture
// containing only the rows relevant to a given set of stops, and cross-references
// the captured realtime feed to report how much live data those stops carry. It
// is a one-shot fixture builder, run by hand:
//
//	go run ./tools/maketestdata \
//	    -in /tmp/tfi_gtfs_static.zip \
//	    -out gtfs/testdata/gtfs_static.zip \
//	    -feed gtfs/testdata/tripupdates.gtfsr \
//	    -stops 478,2808,999126
//
// The trimmed ZIP reproduces the same StaticDB (for our stops) as the full feed,
// so buildFromZIPFile can be exercised on a committable fixture, and the golden
// end-to-end test has real scheduled + realtime data to assert against.
package main

import (
	"archive/zip"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"tfi-display/gtfs"
)

func main() {
	in := flag.String("in", "/tmp/tfi_gtfs_static.zip", "full GTFS static ZIP")
	out := flag.String("out", "gtfs/testdata/gtfs_static.zip", "trimmed output ZIP")
	feedPath := flag.String("feed", "gtfs/testdata/tripupdates.gtfsr", "captured realtime feed")
	stopsCSV := flag.String("stops", "478,2808,999126", "comma-separated stop_codes to keep")
	flag.Parse()

	stops := strings.Split(*stopsCSV, ",")
	stopKeep := map[string]bool{}
	for _, s := range stops {
		stopKeep[strings.TrimSpace(s)] = true
	}

	// 1. Build the filtered StaticDB from the full feed — this is the same logic the
	// app uses, and gives us the exact trip set serving our stops.
	fmt.Println("parsing full ZIP (this reads the 306MB stop_times.txt, ~30-60s)…")
	db, err := gtfs.BuildFromZIPFile(*in, stops)
	if err != nil {
		fail("building StaticDB: %v", err)
	}
	fmt.Printf("StaticDB: %d stops, %d trips, %d services\n", len(db.StopTimes), len(db.Trips), len(db.Services))
	for code := range stopKeep {
		n := 0
		for _, byHour := range db.StopTimes[code] {
			n += len(byHour)
		}
		fmt.Printf("  stop %-8s: %d scheduled stop_times, name=%q platform=%q\n",
			code, n, db.StopNames[code], db.StopPlatforms[code])
	}

	// 2. Cross-reference the captured realtime feed against our trip/stop set.
	feedStats(*feedPath, db, stopKeep)

	// 3. Trim the ZIP. Seeds: the trip set (db.Trips keys) and the stop set.
	tripKeep := map[string]bool{}
	for id := range db.Trips {
		tripKeep[id] = true
	}
	if err := trimZIP(*in, *out, stopKeep, tripKeep); err != nil {
		fail("trimming ZIP: %v", err)
	}

	// 4. Sanity-check the trimmed ZIP reproduces the same trip set.
	db2, err := gtfs.BuildFromZIPFile(*out, stops)
	if err != nil {
		fail("re-parsing trimmed ZIP: %v", err)
	}
	fi, _ := os.Stat(*out)
	fmt.Printf("\ntrimmed ZIP: %s (%d bytes)\n", *out, fi.Size())
	fmt.Printf("re-parsed: %d stops, %d trips, %d services (orig %d/%d/%d)\n",
		len(db2.StopTimes), len(db2.Trips), len(db2.Services),
		len(db.StopTimes), len(db.Trips), len(db.Services))
	if len(db2.Trips) != len(db.Trips) {
		fmt.Println("WARNING: trip counts differ between full and trimmed — investigate")
	} else {
		fmt.Println("OK: trimmed fixture reproduces the same trip set")
	}
}

func feedStats(feedPath string, db *gtfs.StaticDB, stopKeep map[string]bool) {
	data, err := os.ReadFile(feedPath)
	if err != nil {
		fmt.Printf("(skip feed cross-ref: %v)\n", err)
		return
	}
	feed := &gtfsrt.FeedMessage{}
	if err := proto.Unmarshal(data, feed); err != nil {
		fmt.Printf("(skip feed cross-ref: unmarshal: %v)\n", err)
		return
	}

	var schedHits, cancelHits, addHits int
	for _, e := range feed.Entity {
		tu := e.GetTripUpdate()
		if tu == nil {
			continue
		}
		tripID := tu.GetTrip().GetTripId()
		rel := int(tu.GetTrip().GetScheduleRelationship())
		switch rel {
		case 3: // cancelled
			if _, ok := db.Trips[tripID]; ok {
				cancelHits++
			}
		case 1: // added
			for _, stu := range tu.StopTimeUpdate {
				sid := stu.GetStopId()
				// resolve via StopNames (stop_code) or StopIDToNumber
				code := sid
				if _, ok := db.StopNames[sid]; !ok {
					if num, ok := db.StopIDToNumber[sid]; ok {
						code = num
					}
				}
				if stopKeep[code] {
					addHits++
				}
			}
		default: // scheduled
			if _, ok := db.Trips[tripID]; ok {
				schedHits++
			}
		}
	}
	fmt.Println("realtime feed cross-reference (entities touching our stops):")
	fmt.Printf("  scheduled trips with live data: %d\n", schedHits)
	fmt.Printf("  cancellations:                  %d\n", cancelHits)
	fmt.Printf("  added-trip stop arrivals:       %d\n", addHits)
}

// trimZIP streams the full GTFS ZIP and writes a smaller one containing only rows
// relevant to the kept stops/trips. shapes.txt and translations.txt are dropped.
func trimZIP(inPath, outPath string, stopKeep, tripKeep map[string]bool) error {
	rc, err := zip.OpenReader(inPath)
	if err != nil {
		return err
	}
	defer rc.Close()
	files := map[string]*zip.File{}
	for _, f := range rc.File {
		files[f.Name] = f
	}

	outF, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer outF.Close()
	zw := zip.NewWriter(outF)
	defer zw.Close()

	// Collected as we go, so later files can filter on them.
	stopIDKeep := map[string]bool{}     // stop_id values mapping to our stop_codes
	routeKeep := map[string]bool{}      // route_ids used by kept trips
	serviceKeep := map[string]bool{}    // service_ids used by kept trips

	// stops.txt: keep rows whose stop_code (fallback stop_id) is in stopKeep.
	if err := filterCSV(files, zw, "stops.txt", func(h map[string]int, row []string) bool {
		code := col(row, h, "stop_code")
		id := col(row, h, "stop_id")
		if code == "" || code == "0" {
			code = id
		}
		if stopKeep[code] {
			stopIDKeep[id] = true
			return true
		}
		return false
	}); err != nil {
		return err
	}

	// trips.txt: keep rows whose trip_id is in tripKeep; record route_id + service_id.
	if err := filterCSV(files, zw, "trips.txt", func(h map[string]int, row []string) bool {
		if tripKeep[col(row, h, "trip_id")] {
			routeKeep[col(row, h, "route_id")] = true
			serviceKeep[col(row, h, "service_id")] = true
			return true
		}
		return false
	}); err != nil {
		return err
	}

	// stop_times.txt (big): keep rows for our trips AT our stops only.
	if err := filterCSV(files, zw, "stop_times.txt", func(h map[string]int, row []string) bool {
		return tripKeep[col(row, h, "trip_id")] && stopIDKeep[col(row, h, "stop_id")]
	}); err != nil {
		return err
	}

	// routes.txt: keep routes used by kept trips.
	if err := filterCSV(files, zw, "routes.txt", func(h map[string]int, row []string) bool {
		return routeKeep[col(row, h, "route_id")]
	}); err != nil {
		return err
	}

	// calendar.txt / calendar_dates.txt: keep services used by kept trips.
	for _, name := range []string{"calendar.txt", "calendar_dates.txt"} {
		if err := filterCSV(files, zw, name, func(h map[string]int, row []string) bool {
			return serviceKeep[col(row, h, "service_id")]
		}); err != nil {
			return err
		}
	}

	// agency.txt / feed_info.txt: copy whole (tiny, may be referenced).
	for _, name := range []string{"agency.txt", "feed_info.txt"} {
		if err := copyWhole(files, zw, name); err != nil {
			return err
		}
	}
	return nil
}

// filterCSV reads a named CSV from the ZIP, keeps the header plus rows for which
// keep() returns true, and writes the result into the output ZIP.
func filterCSV(files map[string]*zip.File, zw *zip.Writer, name string, keep func(h map[string]int, row []string) bool) error {
	f, ok := files[name]
	if !ok {
		return nil // optional file absent — fine
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	r := csv.NewReader(src)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	header, err := r.Read()
	if err != nil {
		return fmt.Errorf("%s header: %w", name, err)
	}
	h := map[string]int{}
	for i, c := range header {
		h[strings.TrimPrefix(strings.TrimSpace(c), "\xef\xbb\xbf")] = i
	}

	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	kept := 0
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s row: %w", name, err)
		}
		if keep(h, row) {
			if err := cw.Write(row); err != nil {
				return err
			}
			kept++
		}
	}
	cw.Flush()
	fmt.Printf("  trimmed %-20s → %d rows\n", name, kept)
	return cw.Error()
}

func copyWhole(files map[string]*zip.File, zw *zip.Writer, name string) error {
	f, ok := files[name]
	if !ok {
		return nil
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

func col(row []string, h map[string]int, name string) string {
	if i, ok := h[name]; ok && i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
