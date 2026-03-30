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
}

// NewLiveStore returns an initialised LiveStore.
func NewLiveStore() *LiveStore {
	return &LiveStore{
		Delays:        make(map[string][]StopDelay),
		Cancellations: make(map[string]time.Time),
		Additions:     make(map[string][]Addition),
	}
}

// FeedTime returns the timestamp of the last successful live data fetch.
func (s *LiveStore) FeedTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.LastFeedTime
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
	if time.Since(t) >= 24*time.Hour {
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
	url   string
	apiKey string
	db    *StaticDB // needed to resolve stop_id → stop_number
	store *LiveStore

	rateLimitCount int
}

// NewPoller creates a Poller for the given GTFS-RT endpoint.
func NewPoller(url, apiKey string, db *StaticDB) *Poller {
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

// parse unmarshals a GTFS-RT protobuf and updates the LiveStore.
func (p *Poller) parse(data []byte) error {
	feed := &gtfsrt.FeedMessage{}
	if err := proto.Unmarshal(data, feed); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	feedTS := int64(0)
	if feed.Header != nil {
		feedTS = int64(feed.Header.GetTimestamp())
	}
	feedTime := time.Unix(feedTS, 0)

	// We build new maps and swap them atomically to avoid partial updates.
	newDelays := make(map[string][]StopDelay)
	newCancels := make(map[string]time.Time)
	newAdds := make(map[string][]Addition)

	// Preserve old cancellations that are still within 24h.
	p.store.mu.RLock()
	for id, t := range p.store.Cancellations {
		if time.Since(t) < 24*time.Hour {
			newCancels[id] = t
		}
	}
	p.store.mu.RUnlock()

	nUpdates, nAdded, nCancelled, nUnknown := 0, 0, 0, 0

	for _, entity := range feed.Entity {
		tu := entity.GetTripUpdate()
		if tu == nil {
			continue
		}
		tripID := tu.GetTrip().GetTripId()
		rel := int(tu.GetTrip().GetScheduleRelationship())

		switch rel {
		case tripCancelled:
			newCancels[tripID] = feedTime
			nCancelled++
			continue
		case tripAdded:
			// Handle added trips: we need an arrival time for each stop.
			routeID := tu.GetTrip().GetRouteId()
			for _, stu := range tu.StopTimeUpdate {
				if stu.GetArrival().GetTime() == 0 {
					continue
				}
				stopNumber := p.resolveStopID(stu.GetStopId())
				if stopNumber == "" {
					continue
				}
				// Resolve route short name from our static data.
				routeShort := p.routeShortName(routeID)
				arr := Addition{
					RouteShortName: routeShort,
					ArrivalTime:    time.Unix(stu.GetArrival().GetTime(), 0),
					FeedTimestamp:  feedTS,
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

		// Scheduled trip.
		if _, ok := p.db.Trips[tripID]; !ok {
			nUnknown++
			continue
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
func (p *Poller) resolveStopID(stopID string) string {
	// If the stopID is directly in our StopNames (i.e. it is a stop number), use it.
	if _, ok := p.db.StopNames[stopID]; ok {
		return stopID
	}
	// Check if any of our stops have this as their ID (heuristic: TFI uses numeric IDs).
	// As a fallback, return the stopID itself and let the caller filter.
	return stopID
}

// routeShortName returns the short name for a route_id from the static data.
func (p *Poller) routeShortName(routeID string) string {
	if name, ok := p.db.RouteShortNames[routeID]; ok {
		return name
	}
	return routeID
}
