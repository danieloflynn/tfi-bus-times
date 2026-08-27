package gtfs

import (
	"archive/zip"
	"encoding/csv"
	"encoding/gob"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const schemaVer = 3

// gtfsDayNames is the calendar.txt weekday column order (GTFS Monday-first).
// Package-level so the calendar row callback doesn't allocate it per row.
var gtfsDayNames = [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

// StopTime represents one scheduled visit of a trip to a stop.
type StopTime struct {
	TripID       string
	ArrivalSecs  int // seconds since midnight; may exceed 86400 for overnight trips
	StopSequence int
}

// Trip holds route and service info for a trip.
type Trip struct {
	RouteShort string
	ServiceID  string
	Headsign   string
}

// Service represents a calendar entry.
type Service struct {
	StartDate time.Time
	EndDate   time.Time
	Days      [7]bool // index 0=Monday … 6=Sunday (GTFS order)
}

// StaticDB is the parsed, filtered GTFS static dataset persisted as gob.
type StaticDB struct {
	// stopNumber → hour-of-day (0-23) → []StopTime
	StopTimes map[string]map[int][]StopTime
	Trips     map[string]Trip    // tripID → Trip
	Services  map[string]Service // serviceID → Service
	// "serviceID:YYYYMMDD" → 1 (added) or 2 (removed)
	Exceptions      map[string]int
	StopNames       map[string]string // stopNumber → human name
	StopPlatforms   map[string]string // stopNumber → platform_code (empty if not present)
	RouteShortNames map[string]string // routeID → route_short_name
	// StopIDToNumber maps a GTFS stop_id to its stop_number (stop_code), but
	// only for our configured stops and only where the two differ. The realtime
	// feed sometimes identifies a stop by its raw stop_id rather than the
	// stop_code we key everything else by (notably for rail), so added trips
	// would otherwise be filed under an unrecognised stop and never shown.
	StopIDToNumber map[string]string
	Timestamp       time.Time         // ZIP Last-Modified (used for cache invalidation)
	FilterStops     []string          // sorted list of stop numbers used during build
	SchemaVer       int
}

// LoadOrBuild returns a StaticDB, loading from the gob cache if it is fresh,
// or downloading + parsing the GTFS ZIP otherwise.
func LoadOrBuild(staticURL, dataDir string, filterStops []string) (*StaticDB, error) {
	sort.Strings(filterStops)
	cachePath := filepath.Join(dataDir, "static_cache.gob")

	// Try loading existing cache.
	if db, err := loadGob(cachePath); err == nil {
		if db.SchemaVer == schemaVer && slicesEqual(db.FilterStops, filterStops) {
			slog.Info("static cache loaded", "stops", len(db.StopTimes), "trips", len(db.Trips))
			// Still check if a newer ZIP exists.
			if !isNewerZIPAvailable(staticURL, db.Timestamp) {
				return db, nil
			}
			slog.Info("newer static data available, rebuilding")
		} else {
			slog.Info("cache schema/filter mismatch, rebuilding")
		}
	}

	// Download and parse.
	db, err := buildFromURL(staticURL, filterStops)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	if err := saveGob(cachePath, db); err != nil {
		slog.Warn("failed to save gob cache", "err", err)
	}
	return db, nil
}

// MaybeRebuild is the running-process counterpart to LoadOrBuild's startup
// freshness check. It re-checks the upstream GTFS static feed and, when a newer
// ZIP is available (compared against currentTimestamp via the Last-Modified
// header), downloads + parses it and persists the refreshed gob cache. Returns:
//
//   - (db, nil)  when a newer feed was downloaded and parsed;
//   - (nil, nil) when the cache is already current (nothing to do);
//   - (nil, err) when the rebuild failed (the check failing is treated as
//     "nothing to do", consistent with isNewerZIPAvailable's own logging).
//
// Without periodic rebuilding the in-memory dataset is only ever loaded at
// startup, so after TFI republishes the static feed (~weekly) the realtime trip
// IDs stop matching p.db.Trips and every arrival falls back to its scheduled
// time until the process is restarted.
func MaybeRebuild(staticURL, dataDir string, filterStops []string, currentTimestamp time.Time) (*StaticDB, error) {
	if !isNewerZIPAvailable(staticURL, currentTimestamp) {
		return nil, nil
	}
	// Sort to match LoadOrBuild so the persisted FilterStops stays canonical and
	// the cache validates cleanly on the next startup.
	sort.Strings(filterStops)
	db, err := buildFromURL(staticURL, filterStops)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	cachePath := filepath.Join(dataDir, "static_cache.gob")
	if err := saveGob(cachePath, db); err != nil {
		slog.Warn("failed to save gob cache", "err", err)
	}
	return db, nil
}

// isNewerZIPAvailable does a HEAD request and compares Last-Modified.
func isNewerZIPAvailable(url string, cached time.Time) bool {
	// Bounded client, not http.Head: an unbounded HEAD that hangs would wedge
	// the background refresher goroutine for the life of the process, and the
	// static dataset would silently go stale (see httpclient.go).
	resp, err := staticHeadClient.Head(url)
	if err != nil {
		slog.Warn("HEAD request failed", "err", err)
		return false
	}
	resp.Body.Close()
	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		return false
	}
	t, err := http.ParseTime(lm)
	if err != nil {
		return false
	}
	return t.After(cached)
}

// buildFromURL downloads the GTFS ZIP and parses it.
func buildFromURL(url string, filterStops []string) (*StaticDB, error) {
	slog.Info("downloading GTFS static data", "url", url)
	resp, err := staticDownloadClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("downloading GTFS: %w", err)
	}
	defer resp.Body.Close()

	// Parse Last-Modified header.
	var zipTime time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		zipTime, _ = http.ParseTime(lm)
	}
	if zipTime.IsZero() {
		zipTime = time.Now().UTC()
	}

	tmp, err := os.CreateTemp("", "gtfs-*.zip")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("downloading GTFS: %w", err)
	}
	tmp.Close()

	return buildFromZIPFile(tmpPath, zipTime, filterStops)
}

// BuildFromZIPFile parses a local GTFS ZIP file (useful for testing).
func BuildFromZIPFile(path string, filterStops []string) (*StaticDB, error) {
	info, _ := os.Stat(path)
	var t time.Time
	if info != nil {
		t = info.ModTime()
	}
	return buildFromZIPFile(path, t, filterStops)
}

func buildFromZIPFile(path string, zipTime time.Time, filterStops []string) (*StaticDB, error) {
	rc, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening ZIP: %w", err)
	}
	defer rc.Close()
	r := &rc.Reader

	db := &StaticDB{
		StopTimes:       make(map[string]map[int][]StopTime),
		Trips:           make(map[string]Trip),
		Services:        make(map[string]Service),
		Exceptions:      make(map[string]int),
		StopNames:       make(map[string]string),
		StopPlatforms:   make(map[string]string),
		RouteShortNames: make(map[string]string),
		StopIDToNumber:  make(map[string]string),
		Timestamp:       zipTime,
		FilterStops:     filterStops,
		SchemaVer:       schemaVer,
	}

	filterSet := make(map[string]bool, len(filterStops))
	for _, s := range filterStops {
		filterSet[s] = true
	}

	// We need to parse files in a specific order (stops first, then routes/calendar,
	// then trips, then stop_times). Read all files into a map first.
	files := make(map[string]*zip.File, len(r.File))
	for _, f := range r.File {
		files[f.Name] = f
	}

	// 1. stops.txt: build stopID → stopNumber, stopNumber → name, stopNumber → platform_code
	stopIDToNumber := make(map[string]string)
	if err := parseCSV(files, "stops.txt", func(header map[string]int, row []string) error {
		stopID := row[header["stop_id"]]
		stopName := row[header["stop_name"]]
		stopCode := row[header["stop_code"]]
		if stopCode == "" || stopCode == "0" {
			stopCode = stopID
		}
		stopIDToNumber[stopID] = stopCode
		db.StopNames[stopCode] = stopName
		// Persist the stop_id → stop_number mapping, but only for the stops we
		// actually watch and only when the id differs from the code (otherwise
		// resolveStopID handles it directly). Scoping to filterStops keeps this
		// map tiny — a handful of entries — rather than carrying every stop in
		// the network in the gob and in memory.
		if stopID != stopCode && (len(filterSet) == 0 || filterSet[stopCode]) {
			db.StopIDToNumber[stopID] = stopCode
		}
		if idx, ok := header["platform_code"]; ok && idx < len(row) {
			if p := strings.TrimSpace(row[idx]); p != "" {
				db.StopPlatforms[stopCode] = p
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing stops.txt: %w", err)
	}

	// 2. routes.txt: build routeID → short name
	if err := parseCSV(files, "routes.txt", func(header map[string]int, row []string) error {
		routeID := row[header["route_id"]]
		shortName := row[header["route_short_name"]]
		db.RouteShortNames[routeID] = shortName
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing routes.txt: %w", err)
	}

	// 3. calendar.txt
	if err := parseCSV(files, "calendar.txt", func(header map[string]int, row []string) error {
		serviceID := row[header["service_id"]]
		start, err := parseGTFSDate(row[header["start_date"]])
		if err != nil {
			return err
		}
		end, err := parseGTFSDate(row[header["end_date"]])
		if err != nil {
			return err
		}
		var days [7]bool
		for i, d := range gtfsDayNames {
			days[i] = row[header[d]] == "1"
		}
		db.Services[serviceID] = Service{StartDate: start, EndDate: end, Days: days}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing calendar.txt: %w", err)
	}

	// 4. calendar_dates.txt
	if err := parseCSV(files, "calendar_dates.txt", func(header map[string]int, row []string) error {
		serviceID := row[header["service_id"]]
		date, err := parseGTFSDate(row[header["date"]])
		if err != nil {
			return err
		}
		exType, _ := strconv.Atoi(row[header["exception_type"]])
		// Build "serviceID:YYYYMMDD" without time.Format (allocation-free append
		// into a stack buffer); one string alloc for the stored map key remains.
		// Matches the exact key IsServiceActive reconstructs on the query path.
		var keyBuf [32]byte
		kb := append(keyBuf[:0], serviceID...)
		kb = append(kb, ':')
		y, mo, dd := date.Date()
		kb = appendYYYYMMDD(kb, y, int(mo), dd)
		db.Exceptions[string(kb)] = exType
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing calendar_dates.txt: %w", err)
	}

	// 5. stop_times.txt: the big one — stream, filter early. Parsed *before*
	// trips.txt so we learn exactly which trips serve our stops and can build
	// db.Trips directly in step 6, without first buffering every trip in the
	// network into an intermediate map. On the full TFI feed that allTrips map
	// (100k+ trips × three strings each) was the build's dominant transient
	// allocation, almost all of it immediately discarded by the stop filter.
	// Collect which tripIDs serve our stops.
	tripsForStops := make(map[string]bool)
	rowCount := 0

	if err := parseCSV(files, "stop_times.txt", func(header map[string]int, row []string) error {
		rowCount++
		stopID := row[header["stop_id"]]
		stopNumber := stopIDToNumber[stopID]
		if len(filterSet) > 0 && !filterSet[stopNumber] {
			return nil
		}

		tripID := row[header["trip_id"]]
		arrivalStr := row[header["arrival_time"]]
		seqStr := row[header["stop_sequence"]]

		arrivalSecs, err := parseGTFSTime(arrivalStr)
		if err != nil {
			return nil // skip malformed rows
		}
		seq, _ := strconv.Atoi(seqStr)

		// Hour bucket uses (arrivalSecs/3600) % 24
		hour := (arrivalSecs / 3600) % 24

		if db.StopTimes[stopNumber] == nil {
			db.StopTimes[stopNumber] = make(map[int][]StopTime)
		}
		db.StopTimes[stopNumber][hour] = append(db.StopTimes[stopNumber][hour], StopTime{
			TripID:       tripID,
			ArrivalSecs:  arrivalSecs,
			StopSequence: seq,
		})
		tripsForStops[tripID] = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing stop_times.txt: %w", err)
	}
	slog.Info("parsed stop_times", "rows", rowCount, "kept_trips", len(tripsForStops))

	// 6. trips.txt: build db.Trips directly, keeping only trips that serve our
	// stops (tripsForStops, learned above). When no stop filter is configured
	// tripsForStops covers every trip, so all are kept — identical to the old
	// behaviour, but without ever materialising the intermediate allTrips map.
	if err := parseCSV(files, "trips.txt", func(header map[string]int, row []string) error {
		tripID := row[header["trip_id"]]
		if len(tripsForStops) > 0 && !tripsForStops[tripID] {
			return nil
		}
		db.Trips[tripID] = Trip{
			RouteShort: db.RouteShortNames[row[header["route_id"]]],
			ServiceID:  row[header["service_id"]],
			Headsign:   row[header["trip_headsign"]],
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing trips.txt: %w", err)
	}

	slog.Info("built StaticDB",
		"stops", len(db.StopTimes),
		"trips", len(db.Trips),
		"services", len(db.Services),
		"exceptions", len(db.Exceptions),
	)
	return db, nil
}

// parseCSV opens a named file from the ZIP and calls fn for each data row.
// The header row is parsed into a name→index map.
func parseCSV(files map[string]*zip.File, name string, fn func(header map[string]int, row []string) error) error {
	f, ok := files[name]
	if !ok {
		// Some feeds omit calendar.txt if they only use calendar_dates.txt.
		slog.Warn("file not found in ZIP", "name", name)
		return nil
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	r := csv.NewReader(rc)
	r.ReuseRecord = true
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	headerRow, err := r.Read()
	if err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	header := make(map[string]int, len(headerRow))
	for i, h := range headerRow {
		// Strip BOM if present on first field.
		h = strings.TrimPrefix(h, "\xef\xbb\xbf")
		header[strings.TrimSpace(h)] = i
	}

	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := fn(header, row); err != nil {
			return err
		}
	}
	return nil
}

// parseGTFSDate parses "YYYYMMDD" (with or without hyphens).
func parseGTFSDate(s string) (time.Time, error) {
	s = strings.ReplaceAll(s, "-", "")
	return time.ParseInLocation("20060102", s, time.UTC)
}

// parseGTFSTime parses "HH:MM:SS" (hours may exceed 23 for overnight trips).
// It locates the two colons by index and parses the three fields from string
// sub-slices rather than strings.Split, which allocated a []string on every
// stop_times row — the largest GTFS file (the dominant build allocation source).
func parseGTFSTime(s string) (int, error) {
	c1 := strings.IndexByte(s, ':')
	if c1 < 0 {
		return 0, fmt.Errorf("invalid time: %q", s)
	}
	c2 := strings.IndexByte(s[c1+1:], ':')
	if c2 < 0 {
		return 0, fmt.Errorf("invalid time: %q", s)
	}
	c2 += c1 + 1 // make c2 an index into s
	h, err := strconv.Atoi(s[:c1])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(s[c1+1 : c2])
	if err != nil {
		return 0, err
	}
	// A third colon (e.g. "10:00:00:00") leaves a non-numeric seconds field, so
	// Atoi rejects it — matching the old len(parts)==3 check.
	ss, err := strconv.Atoi(s[c2+1:])
	if err != nil {
		return 0, err
	}
	return h*3600 + m*60 + ss, nil
}

func saveGob(path string, db *StaticDB) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewEncoder(f).Encode(db)
}

func loadGob(path string) (*StaticDB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var db StaticDB
	if err := gob.NewDecoder(f).Decode(&db); err != nil {
		return nil, err
	}
	return &db, nil
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
