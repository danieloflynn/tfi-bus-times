package gtfs

import (
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Custom streaming decoder for the GTFS-realtime TripUpdates feed.
//
// proto.Unmarshal builds the entire FeedMessage object graph — a heap struct
// for every FeedEntity, TripUpdate, TripDescriptor, StopTimeUpdate and
// StopTimeEvent — even though parse only reads a handful of scalar fields. On a
// real feed (~2900 entities, ~15700 stop-time updates) that is the dominant
// source of allocations and GC pressure on the Pi, repeated every poll.
//
// This decoder walks the protobuf wire format directly with protowire and
// extracts only the fields parse uses, never materialising the object graph.
// Field numbers below are from the canonical gtfs-realtime.proto and are stable.
// Where a string is only ever used as a map *lookup* key (and never stored), we
// keep the raw []byte so the compiler's `m[string(b)]` no-copy optimisation
// avoids the allocation; strings that are stored in maps are materialised then.
const (
	// FeedMessage
	fieldFeedHeader = 1
	fieldFeedEntity = 2
	// FeedHeader
	fieldHeaderTimestamp = 3
	// FeedEntity
	fieldEntityTripUpdate = 3
	// TripUpdate
	fieldTUTrip           = 1
	fieldTUStopTimeUpdate = 2
	// TripDescriptor
	fieldTDTripID      = 1
	fieldTDScheduleRel = 4
	fieldTDRouteID     = 5
	// StopTimeUpdate
	fieldSTUStopSequence = 1
	fieldSTUArrival      = 2
	fieldSTUDeparture    = 3
	fieldSTUStopID       = 4
	fieldSTUScheduleRel  = 5
	// StopTimeEvent
	fieldSTEDelay = 1
	fieldSTETime  = 2
)

// rtStop is a decoded StopTimeUpdate, reused via the decoder's scratch slice so
// no per-stop heap allocation occurs. stopID stays a []byte sub-slice of the
// input (only the rare added-trip path needs it as a string).
type rtStop struct {
	seq      uint32
	rel      int32
	arrTime  int64
	arrDelay int32
	depTime  int64
	stopID   []byte
}

// feedDecoder holds the per-parse state and reusable scratch buffers so that
// decoding a whole feed allocates only the strings/slices that are actually
// stored in the swap maps.
type feedDecoder struct {
	db         *StaticDB
	feedTS     int64
	feedTime   time.Time
	newDelays  map[string][]StopDelay
	newCancels map[string]time.Time
	newAdds    map[string][]Addition
	// watched is the configured stop_number set; nil = watch all. Used to drop
	// ADDED-trip stops outside the board's stops without allocating their id.
	watched map[string]bool

	stops []rtStop // reused across trips, reset per trip

	// delayArena backs every scheduled trip's []StopDelay for this poll. Rather
	// than make() a fresh slice per trip (~1285 small allocations/poll), each
	// trip appends into this one arena and is handed a 3-arg-capped sub-slice.
	// Appends that grow the arena copy forward, but slices already handed out
	// reference whichever backing array was current when taken and stay valid
	// (the swap map keeps that array alive); the cap pin stops a stored slice
	// from ever appending into a neighbour. Pre-sized from the previous poll's
	// total, so a stable feed reallocates ~once.
	delayArena []StopDelay

	nUpdates, nAdded, nCancelled, nUnknown int
}

// decodeFeed walks the top-level FeedMessage, extracting the header timestamp
// and dispatching each entity's TripUpdate. It returns an error (never panics)
// on malformed wire data so parse stays fuzz-safe.
func (d *feedDecoder) decodeFeed(b []byte) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("decode feed tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch {
		case num == fieldFeedHeader && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return fmt.Errorf("decode header: %w", protowire.ParseError(m))
			}
			if err := d.decodeHeader(v); err != nil {
				return err
			}
			b = b[m:]
		case num == fieldFeedEntity && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return fmt.Errorf("decode entity: %w", protowire.ParseError(m))
			}
			if err := d.decodeEntity(v); err != nil {
				return err
			}
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return fmt.Errorf("skip feed field: %w", protowire.ParseError(m))
			}
			b = b[m:]
		}
	}
	return nil
}

// decodeHeader extracts the feed timestamp (FeedHeader field 3).
func (d *feedDecoder) decodeHeader(b []byte) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("decode header tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		if num == fieldHeaderTimestamp && typ == protowire.VarintType {
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return fmt.Errorf("decode header ts: %w", protowire.ParseError(m))
			}
			d.feedTS = int64(v)
			d.feedTime = time.Unix(d.feedTS, 0)
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return fmt.Errorf("skip header field: %w", protowire.ParseError(m))
		}
		b = b[m:]
	}
	return nil
}

// decodeEntity finds the TripUpdate (FeedEntity field 3) and processes it.
func (d *feedDecoder) decodeEntity(b []byte) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("decode entity tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		if num == fieldEntityTripUpdate && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return fmt.Errorf("decode trip_update: %w", protowire.ParseError(m))
			}
			if err := d.decodeTripUpdate(v); err != nil {
				return err
			}
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return fmt.Errorf("skip entity field: %w", protowire.ParseError(m))
		}
		b = b[m:]
	}
	return nil
}

// decodeTripUpdate mirrors the per-entity switch in parse. It scans the
// TripUpdate buffer twice — once for the TripDescriptor (so the schedule
// relationship is known before stops are interpreted, regardless of wire
// order) and once for the StopTimeUpdates — both linear over a small buffer.
func (d *feedDecoder) decodeTripUpdate(b []byte) error {
	var tripID, routeID []byte
	var rel int32

	// Pass 1: TripDescriptor (TripUpdate field 1).
	for rest := b; len(rest) > 0; {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return fmt.Errorf("decode tu tag: %w", protowire.ParseError(n))
		}
		rest = rest[n:]
		if num == fieldTUTrip && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(rest)
			if m < 0 {
				return fmt.Errorf("decode trip: %w", protowire.ParseError(m))
			}
			var err error
			if tripID, routeID, rel, err = decodeTripDescriptor(v); err != nil {
				return err
			}
			rest = rest[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, rest)
		if m < 0 {
			return fmt.Errorf("skip tu field: %w", protowire.ParseError(m))
		}
		rest = rest[m:]
	}

	switch int(rel) {
	case tripCancelled:
		d.newCancels[string(tripID)] = d.feedTime
		d.nCancelled++
		return nil
	case tripAdded:
		return d.handleAdded(b, routeID)
	}

	// Scheduled trip. Use the no-copy map lookup; only materialise the trip ID
	// string when it is actually stored in newDelays.
	if _, ok := d.db.Trips[string(tripID)]; !ok {
		d.nUnknown++
		return nil
	}
	if err := d.collectStops(b); err != nil {
		return err
	}

	start := len(d.delayArena)
	for i := range d.stops {
		stu := &d.stops[i]
		if int(stu.rel) == stopSkipped {
			continue
		}
		var sd StopDelay
		sd.StopSequence = int32(stu.seq)
		if stu.arrTime != 0 {
			sd.AbsTime = stu.arrTime
		} else {
			dl := stu.arrDelay
			if dl > maxDelaySeconds || dl < -maxDelaySeconds {
				continue // discard implausible delays
			}
			sd.DelaySeconds = dl
		}
		d.delayArena = append(d.delayArena, sd)
		d.nUpdates++
	}
	if end := len(d.delayArena); end > start {
		// Cap the sub-slice at its length so the stored slice can never append
		// into the next trip's region of the arena.
		delays := d.delayArena[start:end:end]
		sortStopDelays(delays)
		d.newDelays[string(tripID)] = delays
	}
	return nil
}

// handleAdded reproduces the ADDED-trip stop handling from parse: each stop
// becomes an Addition keyed by resolved stop number, with per-route dedup.
func (d *feedDecoder) handleAdded(tuBuf, routeID []byte) error {
	routeShort, routeResolved := routeShortName(d.db, string(routeID))
	if err := d.collectStops(tuBuf); err != nil {
		return err
	}
	for i := range d.stops {
		stu := &d.stops[i]
		// Prefer arrival, fall back to departure: added stops sometimes carry
		// only a departure event and dropping them would hide replacements.
		arrTS := stu.arrTime
		if arrTS == 0 {
			arrTS = stu.depTime
		}
		if arrTS == 0 {
			continue
		}
		// Resolve to a watched stop_number without allocating string(stu.stopID)
		// for stops we don't watch. The feed carries ADDED trips network-wide
		// (thousands of stop-updates/poll); QueryArrivals only ever reads the
		// configured stops, and this string conversion was ~94% of the parse's
		// allocations. The no-copy m[string(b)] lookups don't allocate; the key
		// is materialised only when an Addition is actually stored.
		stopNumber, ok := d.resolveWatchedStop(stu.stopID)
		if !ok {
			continue
		}
		arr := Addition{
			RouteShortName: routeShort,
			ArrivalTime:    time.Unix(arrTS, 0),
			FeedTimestamp:  d.feedTS,
			RouteResolved:  routeResolved,
		}
		existing := d.newAdds[stopNumber]
		deduped := existing[:0]
		for _, a := range existing {
			if a.RouteShortName == routeShort && a.ArrivalTime.Before(arr.ArrivalTime) {
				continue // drop old entry for same route
			}
			deduped = append(deduped, a)
		}
		d.newAdds[stopNumber] = append(deduped, arr)
		d.nAdded++
	}
	return nil
}

// collectStops decodes every StopTimeUpdate (TripUpdate field 2) into the
// reused d.stops scratch slice.
func (d *feedDecoder) collectStops(b []byte) error {
	d.stops = d.stops[:0]
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("decode stop tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		if num == fieldTUStopTimeUpdate && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return fmt.Errorf("decode stop: %w", protowire.ParseError(m))
			}
			stu, err := decodeStopTimeUpdate(v)
			if err != nil {
				return err
			}
			d.stops = append(d.stops, stu)
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return fmt.Errorf("skip stop field: %w", protowire.ParseError(m))
		}
		b = b[m:]
	}
	return nil
}

// decodeTripDescriptor extracts trip_id, route_id and schedule_relationship.
// trip_id and route_id are returned as sub-slices of the input (no allocation).
func decodeTripDescriptor(b []byte) (tripID, routeID []byte, rel int32, err error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, nil, 0, fmt.Errorf("decode td tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch {
		case num == fieldTDTripID && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, nil, 0, fmt.Errorf("decode trip_id: %w", protowire.ParseError(m))
			}
			tripID = v
			b = b[m:]
		case num == fieldTDRouteID && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, nil, 0, fmt.Errorf("decode route_id: %w", protowire.ParseError(m))
			}
			routeID = v
			b = b[m:]
		case num == fieldTDScheduleRel && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return nil, nil, 0, fmt.Errorf("decode trip rel: %w", protowire.ParseError(m))
			}
			rel = int32(v)
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return nil, nil, 0, fmt.Errorf("skip td field: %w", protowire.ParseError(m))
			}
			b = b[m:]
		}
	}
	return tripID, routeID, rel, nil
}

// decodeStopTimeUpdate extracts the scalar fields parse needs from one
// StopTimeUpdate, descending into the arrival/departure StopTimeEvents.
func decodeStopTimeUpdate(b []byte) (rtStop, error) {
	var s rtStop
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return s, fmt.Errorf("decode stu tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch {
		case num == fieldSTUStopSequence && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return s, fmt.Errorf("decode stop_sequence: %w", protowire.ParseError(m))
			}
			s.seq = uint32(v)
			b = b[m:]
		case num == fieldSTUScheduleRel && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return s, fmt.Errorf("decode stop rel: %w", protowire.ParseError(m))
			}
			s.rel = int32(v)
			b = b[m:]
		case num == fieldSTUStopID && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return s, fmt.Errorf("decode stop_id: %w", protowire.ParseError(m))
			}
			s.stopID = v
			b = b[m:]
		case num == fieldSTUArrival && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return s, fmt.Errorf("decode arrival: %w", protowire.ParseError(m))
			}
			t, dl, err := decodeStopTimeEvent(v)
			if err != nil {
				return s, err
			}
			s.arrTime, s.arrDelay = t, dl
			b = b[m:]
		case num == fieldSTUDeparture && typ == protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return s, fmt.Errorf("decode departure: %w", protowire.ParseError(m))
			}
			t, _, err := decodeStopTimeEvent(v)
			if err != nil {
				return s, err
			}
			s.depTime = t
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return s, fmt.Errorf("skip stu field: %w", protowire.ParseError(m))
			}
			b = b[m:]
		}
	}
	return s, nil
}

// decodeStopTimeEvent extracts the time and delay scalars from a StopTimeEvent.
func decodeStopTimeEvent(b []byte) (t int64, delay int32, err error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, 0, fmt.Errorf("decode ste tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch {
		case num == fieldSTETime && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return 0, 0, fmt.Errorf("decode ste time: %w", protowire.ParseError(m))
			}
			t = int64(v)
			b = b[m:]
		case num == fieldSTEDelay && typ == protowire.VarintType:
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return 0, 0, fmt.Errorf("decode ste delay: %w", protowire.ParseError(m))
			}
			delay = int32(v)
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return 0, 0, fmt.Errorf("skip ste field: %w", protowire.ParseError(m))
			}
			b = b[m:]
		}
	}
	return t, delay, nil
}

// sortStopDelays sorts a per-trip delay slice ascending by StopSequence for the
// binary search in GetDelay. slices.SortFunc avoids sort.Slice's reflection.
func sortStopDelays(delays []StopDelay) {
	slices.SortFunc(delays, func(a, b StopDelay) int {
		return int(a.StopSequence - b.StopSequence)
	})
}
