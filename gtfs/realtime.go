package gtfs

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// StopDelay holds realtime delay data for one stop within a trip.
type StopDelay struct {
	StopSequence int32
	// AbsTime is an absolute Unix timestamp for the arrival (non-zero = preferred).
	AbsTime int64
	// DelaySeconds is used when AbsTime == 0 (positive = late, negative = early).
	DelaySeconds int32
}

// Addition is a realtime-added trip arrival at a stop.
type Addition struct {
	RouteShortName string
	ArrivalTime    time.Time
	FeedTimestamp  int64
	// RouteResolved is true when RouteShortName came from the static feed. When
	// false the route_id was not in routes.txt (e.g. a brand-new disruption
	// route) and RouteShortName holds the raw id; QueryArrivals shows such
	// additions even under a route whitelist rather than dropping them.
	RouteResolved bool
}

// LiveStore holds in-memory realtime data protected by a RWMutex.
type LiveStore struct {
	mu sync.RWMutex
	// tripID → []StopDelay sorted ascending by StopSequence
	Delays map[string][]StopDelay
	// tripID → time the cancellation was received
	Cancellations map[string]time.Time
	// stopNumber → []Addition
	Additions    map[string][]Addition
	LastFeedTime time.Time
	LastPollTime time.Time
	// diagLogged deduplicates one-shot diagnostic ("rescue") logs so they fire at
	// most once per poll cycle instead of on every render/page tick. It is reset
	// each time a fresh feed is parsed.
	diagLogged map[string]bool
	// now is the clock used for the 24h cancellation window and poll timestamps.
	// Defaults to time.Now; tests substitute a fixed clock so time-dependent
	// branches are deterministic. Set once before use — never reassigned while
	// other goroutines read it.
	now func() time.Time
}

// NewLiveStore returns an initialised LiveStore.
func NewLiveStore() *LiveStore {
	return &LiveStore{
		Delays:        make(map[string][]StopDelay),
		Cancellations: make(map[string]time.Time),
		Additions:     make(map[string][]Addition),
		diagLogged:    make(map[string]bool),
		now:           time.Now,
	}
}

// DiagLogOnce reports whether key has not yet been logged for the current feed
// snapshot, recording it so subsequent calls in the same poll cycle return
// false. It lets QueryArrivals (called on every render and page tick) emit
// rescue diagnostics at most once per poll without spamming the log.
func (ls *LiveStore) DiagLogOnce(key string) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.diagLogged[key] {
		return false
	}
	ls.diagLogged[key] = true
	return true
}

// FeedTime returns the feed header timestamp from the last successful parse.
func (s *LiveStore) FeedTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastFeedTime
}

// PollTime returns the wall-clock time of the last successful poll.
func (s *LiveStore) PollTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastPollTime
}

// GetDelay returns the StopDelay for tripID at or before stopSequence using
// binary search. Returns (StopDelay{}, false) if no realtime data is available.
func (ls *LiveStore) GetDelay(tripID string, stopSequence int) (StopDelay, bool) {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	delays, ok := ls.Delays[tripID]
	if !ok || len(delays) == 0 {
		return StopDelay{}, false
	}

	// Binary search: find the largest StopSequence ≤ stopSequence.
	lo, hi := 0, len(delays)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if int(delays[mid].StopSequence) < stopSequence {
			lo = mid + 1
		} else if int(delays[mid].StopSequence) > stopSequence {
			hi = mid - 1
		} else {
			// Exact match.
			return delays[mid], true
		}
	}
	// lo is the first index with StopSequence > stopSequence.
	if lo == 0 {
		return StopDelay{}, false
	}
	return delays[lo-1], true
}

// IsCancelled returns true if tripID was cancelled within the last 24 hours.
func (ls *LiveStore) IsCancelled(tripID string) bool {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	t, ok := ls.Cancellations[tripID]
	if !ok {
		return false
	}
	if ls.now().Sub(t) >= 24*time.Hour {
		return false
	}
	return true
}

// GetAdditions returns added trips for a stop (caller must not mutate the slice).
func (ls *LiveStore) GetAdditions(stopNumber string) []Addition {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.Additions[stopNumber]
}

// Poller polls the GTFS-RT endpoint on a ticker and updates the LiveStore.
type Poller struct {
	url    string
	apiKey string
	// db is a holder, not a fixed *StaticDB: the static dataset is rebuilt in the
	// background while the process runs, so each parse loads the current snapshot.
	db    *DB
	store *LiveStore

	rateLimitCount int
}

// NewPoller creates a Poller for the given GTFS-RT endpoint. db is a holder so
// the poller picks up background static-data refreshes without being recreated.
func NewPoller(url, apiKey string, db *DB) *Poller {
	return &Poller{
		url:    url,
		apiKey: apiKey,
		db:     db,
		store:  NewLiveStore(),
	}
}

// Store returns the managed LiveStore.
func (p *Poller) Store() *LiveStore {
	return p.store
}

// Poll performs one fetch-and-parse cycle. Returns the number of consecutive
// rate-limit errors so the caller can apply backoff.
func (p *Poller) Poll() int {
	data, err := p.fetch()
	if err != nil {
		// fetch already logs
		return p.rateLimitCount
	}
	p.rateLimitCount = 0
	if err := p.parse(data); err != nil {
		slog.Error("parsing realtime feed", "err", err)
	} else {
		p.store.mu.Lock()
		p.store.LastPollTime = p.store.now()
		p.store.mu.Unlock()
	}
	return 0
}

type rateLimitError struct{}

func (rateLimitError) Error() string { return "rate limited (429)" }

func (p *Poller) fetch() ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, p.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("realtime fetch", "err", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		p.rateLimitCount++
		slog.Warn("rate limited", "count", p.rateLimitCount,
			"backoff_s", math.Pow(2, float64(p.rateLimitCount)))
		return nil, rateLimitError{}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// BackoffDuration returns the exponential backoff duration for the current rate
// limit count (0 = no backoff).
func (p *Poller) BackoffDuration(baseSec int) time.Duration {
	if p.rateLimitCount == 0 {
		return 0
	}
	secs := float64(baseSec) * math.Pow(2, float64(p.rateLimitCount-1))
	if secs > 3600 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second
}

const (
	tripScheduled   = 0
	tripAdded       = 1
	tripCancelled   = 3
	stopScheduled   = 0
	stopSkipped     = 1
	maxDelaySeconds = 604800 // one week
)

// GTFS-realtime protobuf field numbers (fixed by the spec). parse uses these to
// read a trip's identity off the wire without fully decoding every trip-update.
const (
	fmHeaderField     = 1 // FeedMessage.header
	fmEntityField     = 2 // FeedMessage.entity
	fhTimestampField  = 3 // FeedHeader.timestamp
	feTripUpdateField = 3 // FeedEntity.trip_update
	tuTripField       = 1 // TripUpdate.trip
	tdTripIDField     = 1 // TripDescriptor.trip_id
	tdRelField        = 4 // TripDescriptor.schedule_relationship
)

// wireFindBytes returns the contents of the first length-delimited field `field`
// in the protobuf message bytes msg, or nil if absent or malformed. It is used to
// descend into submessages (entity → trip_update → trip) without allocating the
// decoded message tree.
func wireFindBytes(msg []byte, field protowire.Number) []byte {
	b := msg
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil
		}
		b = b[n:]
		if num == field && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil
			}
			return v
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return nil
		}
		b = b[m:]
	}
	return nil
}

// wireFindVarint returns the value of the first varint field `field` in msg and
// whether it was present.
func wireFindVarint(msg []byte, field protowire.Number) (uint64, bool) {
	b := msg
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, false
		}
		b = b[n:]
		if num == field && typ == protowire.VarintType {
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return 0, false
			}
			return v, true
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return 0, false
		}
		b = b[m:]
	}
	return 0, false
}

// parse unmarshals a GTFS-RT protobuf and updates the LiveStore.
func (p *Poller) parse(data []byte) error {
	// Snapshot the static dataset once for the whole parse so a mid-parse
	// background refresh can't make trip lookups inconsistent.
	db := p.db.Load()

	// Read the feed header timestamp first. Protobuf does not guarantee field
	// order, so resolve it before stamping cancellations/additions with it.
	feedTS := int64(0)
	if hdr := wireFindBytes(data, fmHeaderField); hdr != nil {
		if ts, ok := wireFindVarint(hdr, fhTimestampField); ok {
			feedTS = int64(ts)
		}
	}
	feedTime := time.Unix(feedTS, 0)

	// We build new maps and swap them atomically to avoid partial updates.
	newDelays := make(map[string][]StopDelay)
	newCancels := make(map[string]time.Time)
	newAdds := make(map[string][]Addition)

	// Preserve old cancellations that are still within 24h.
	p.store.mu.RLock()
	for id, t := range p.store.Cancellations {
		if p.store.now().Sub(t) < 24*time.Hour {
			newCancels[id] = t
		}
	}
	p.store.mu.RUnlock()

	nUpdates, nAdded, nCancelled, nUnknown := 0, 0, 0, 0

	// Walk the entities at the wire level. The feed is a FULL_DATASET of every
	// trip in the network (~2,900), but only the few serving our stops (plus
	// added/cancelled trips) matter. We read each entity's trip_id +
	// schedule_relationship straight from the bytes and run the allocation-heavy
	// proto.Unmarshal only on the trip-updates we actually use — turning a
	// ~2,900-message decode into a ~200-message one. See
	// planning/optimizations/001-parse-wire-prefilter.md.
	b := data
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("unmarshal: %w", protowire.ParseError(n))
		}
		b = b[n:]
		if num != fmEntityField || typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return fmt.Errorf("unmarshal: %w", protowire.ParseError(m))
			}
			b = b[m:]
			continue
		}
		ent, m := protowire.ConsumeBytes(b)
		if m < 0 {
			return fmt.Errorf("unmarshal: %w", protowire.ParseError(m))
		}
		b = b[m:]

		// Only trip-update entities carry a trip_update submessage; vehicle and
		// alert entities don't, and are ignored (matches the old tu == nil skip).
		tuBytes := wireFindBytes(ent, feTripUpdateField)
		if tuBytes == nil {
			continue
		}
		// Cheap pre-read of trip_id + schedule_relationship from the trip
		// descriptor, without decoding the stop_time_update tree.
		tripID := ""
		rel := 0
		if trip := wireFindBytes(tuBytes, tuTripField); trip != nil {
			if idb := wireFindBytes(trip, tdTripIDField); idb != nil {
				tripID = string(idb)
			}
			if r, ok := wireFindVarint(trip, tdRelField); ok {
				rel = int(r)
			}
		}

		switch rel {
		case tripCancelled:
			// Cancellation needs only the trip_id — never decode its stops.
			newCancels[tripID] = feedTime
			nCancelled++
			continue
		case tripAdded:
			tu := &gtfsrt.TripUpdate{}
			if err := proto.Unmarshal(tuBytes, tu); err != nil {
				return fmt.Errorf("unmarshal trip_update: %w", err)
			}
			// Handle added trips: we need an arrival time for each stop.
			routeID := tu.GetTrip().GetRouteId()
			// Resolve route short name from our static data once per trip.
			routeShort, routeResolved := routeShortName(db, routeID)
			for _, stu := range tu.StopTimeUpdate {
				// Prefer the arrival time, but fall back to the departure time:
				// added stops sometimes carry only a departure event, and
				// dropping them would hide the very replacement services that
				// appear during disruptions.
				arrTS := stu.GetArrival().GetTime()
				if arrTS == 0 {
					arrTS = stu.GetDeparture().GetTime()
				}
				if arrTS == 0 {
					continue
				}
				stopNumber := resolveStopID(db, stu.GetStopId())
				if stopNumber == "" {
					continue
				}
				arr := Addition{
					RouteShortName: routeShort,
					ArrivalTime:    time.Unix(arrTS, 0),
					FeedTimestamp:  feedTS,
					RouteResolved:  routeResolved,
				}
				// Deduplicate: remove old entries for the same route at this stop.
				existing := newAdds[stopNumber]
				deduped := existing[:0]
				for _, a := range existing {
					if a.RouteShortName == routeShort && a.ArrivalTime.Before(arr.ArrivalTime) {
						continue // drop old entry for same route
					}
					deduped = append(deduped, a)
				}
				newAdds[stopNumber] = append(deduped, arr)
				nAdded++
			}
			continue
		}

		// Scheduled trip: only fully decode it if it serves one of our stops.
		if _, ok := db.Trips[tripID]; !ok {
			nUnknown++
			continue
		}
		tu := &gtfsrt.TripUpdate{}
		if err := proto.Unmarshal(tuBytes, tu); err != nil {
			return fmt.Errorf("unmarshal trip_update: %w", err)
		}

		var delays []StopDelay
		for _, stu := range tu.StopTimeUpdate {
			if int(stu.GetScheduleRelationship()) == stopSkipped {
				continue
			}

			var sd StopDelay
			sd.StopSequence = int32(stu.GetStopSequence())

			arr := stu.GetArrival()
			if arr.GetTime() != 0 {
				sd.AbsTime = arr.GetTime()
			} else {
				d := arr.GetDelay()
				if d > maxDelaySeconds || d < -maxDelaySeconds {
					continue // discard implausible delays
				}
				sd.DelaySeconds = d
			}
			delays = append(delays, sd)
			nUpdates++
		}

		if len(delays) > 0 {
			// Sort by StopSequence for binary search later.
			sort.Slice(delays, func(i, j int) bool {
				return delays[i].StopSequence < delays[j].StopSequence
			})
			newDelays[tripID] = delays
		}
	}

	// Atomic swap.
	p.store.mu.Lock()
	p.store.Delays = newDelays
	p.store.Cancellations = newCancels
	p.store.Additions = newAdds
	p.store.LastFeedTime = feedTime
	// Fresh feed: clear the rescue-log dedupe so anomalies in this cycle are
	// reported once.
	p.store.diagLogged = make(map[string]bool)
	p.store.mu.Unlock()

	slog.Debug("realtime parsed",
		"updates", nUpdates, "added", nAdded,
		"cancelled", nCancelled, "unknown", nUnknown,
		"feed_time", feedTime.Format(time.RFC3339),
	)
	return nil
}

// resolveStopID converts a stop_id from the RT feed to a stop_number.
// Many TFI feeds use the stop number directly as the stop_id.
func resolveStopID(db *StaticDB, stopID string) string {
	// If the stopID is directly in our StopNames (i.e. it is a stop number), use it.
	if _, ok := db.StopNames[stopID]; ok {
		return stopID
	}
	// Otherwise the feed used the raw GTFS stop_id; map it back to the
	// stop_number we key everything else by (notably for rail, where the two
	// differ). The map only covers our configured stops.
	if num, ok := db.StopIDToNumber[stopID]; ok {
		return num
	}
	// As a fallback, return the stopID itself and let the caller filter.
	return stopID
}

// routeShortName returns the short name for a route_id from the static data and
// whether it was found. When not found it returns the raw routeID and false so
// callers can tell an unresolved (e.g. brand-new disruption) route apart from a
// genuine short name.
func routeShortName(db *StaticDB, routeID string) (string, bool) {
	if name, ok := db.RouteShortNames[routeID]; ok {
		return name, true
	}
	return routeID, false
}
