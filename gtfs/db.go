package gtfs

import "sync/atomic"

// DB is a concurrency-safe holder for the active StaticDB.
//
// The static GTFS feed is rebuilt periodically while the process runs (TFI
// republishes it roughly weekly, and the trip IDs change when it does), so
// readers must not capture a *StaticDB once and hold it forever — that is what
// caused every arrival to revert to its scheduled time after a week of uptime.
// Both the realtime poller and the render loop call Load() each time they need
// the dataset; the background refresher calls Store() to swap in a freshly built
// one. The swap is a single atomic pointer store, so an in-flight reader always
// sees a complete, self-consistent snapshot (either the old DB or the new one,
// never a half-built mix).
type DB struct {
	ptr atomic.Pointer[StaticDB]
}

// NewDB returns a holder wrapping the given StaticDB.
func NewDB(db *StaticDB) *DB {
	h := &DB{}
	h.ptr.Store(db)
	return h
}

// Load returns the current StaticDB.
func (d *DB) Load() *StaticDB { return d.ptr.Load() }

// Store atomically swaps in a new StaticDB.
func (d *DB) Store(db *StaticDB) { d.ptr.Store(db) }
