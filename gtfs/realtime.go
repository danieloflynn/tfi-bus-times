package gtfs

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
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
	// prevDelayTotal is the total StopDelay count from the previous parse, used
	// to size the decoder's delay arena so a stable feed reallocates it ~once.
	prevDelayTotal int
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

// parse decodes a GTFS-RT protobuf and updates the LiveStore. It uses a custom
// streaming wire-format decoder (realtime_decode.go) rather than
// proto.Unmarshal so it never materialises the full FeedMessage object graph —
// the dominant allocation source on the device every poll.
func (p *Poller) parse(data []byte) error {
	// Snapshot the static dataset once for the whole parse so a mid-parse
	// background refresh can't make trip lookups inconsistent.
	db := p.db.Load()

	// Size-hint the new maps from the previous parse so the swap rarely has to
	// grow/rehash them — feed shape is stable poll-to-poll.
	p.store.mu.RLock()
	newDelays := make(map[string][]StopDelay, len(p.store.Delays))
	newCancels := make(map[string]time.Time, len(p.store.Cancellations))
	newAdds := make(map[string][]Addition, len(p.store.Additions))
	// Preserve old cancellations that are still within 24h.
	for id, t := range p.store.Cancellations {
		if p.store.now().Sub(t) < 24*time.Hour {
			newCancels[id] = t
		}
	}
	p.store.mu.RUnlock()

	d := feedDecoder{
		db:         db,
		newDelays:  newDelays,
		newCancels: newCancels,
		newAdds:    newAdds,
		// Default when the feed carries no header timestamp (TS 0), matching the
		// old proto path's time.Unix(0, 0). decodeHeader overwrites this from the
		// real header, which precedes the entities on the wire.
		feedTime: time.Unix(0, 0),
		// One arena backs all per-trip delay slices this poll; size it from the
		// previous total so a stable feed reallocates it ~once.
		delayArena: make([]StopDelay, 0, p.prevDelayTotal),
	}
	if err := d.decodeFeed(data); err != nil {
		return fmt.Errorf("decode feed: %w", err)
	}
	p.prevDelayTotal = len(d.delayArena)

	feedTime := d.feedTime

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
		"updates", d.nUpdates, "added", d.nAdded,
		"cancelled", d.nCancelled, "unknown", d.nUnknown,
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
