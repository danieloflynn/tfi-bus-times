package main

import (
	"testing"
	"time"
)

func TestFeedIsStale(t *testing.T) {
	started := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	limit := 30 * time.Minute

	tests := []struct {
		name  string
		last  time.Time
		now   time.Time
		limit time.Duration
		want  bool
	}{
		{
			name: "recent poll is fresh",
			last: started.Add(5 * time.Minute),
			now:  started.Add(10 * time.Minute),
			want: false,
		},
		{
			name: "poll exactly at the limit is not yet stale",
			last: started,
			now:  started.Add(limit),
			want: false,
		},
		{
			name: "poll beyond the limit is stale",
			last: started,
			now:  started.Add(limit + time.Second),
			want: true,
		},
		{
			// A device that boots onto a dead network never records a poll at all;
			// measuring from process start is what catches it.
			name: "never polled, measured from process start",
			last: time.Time{},
			now:  started.Add(limit + time.Minute),
			want: true,
		},
		{
			name: "never polled but still within the limit",
			last: time.Time{},
			now:  started.Add(limit - time.Minute),
			want: false,
		},
		{
			name:  "zero limit disables the watchdog",
			last:  started,
			now:   started.Add(72 * time.Hour),
			limit: -1,
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := limit
			if tc.limit != 0 {
				l = tc.limit
			}
			if got := feedIsStale(tc.last, started, tc.now, l); got != tc.want {
				t.Errorf("feedIsStale(%v, %v, %v, %v) = %v; want %v",
					tc.last, started, tc.now, l, got, tc.want)
			}
		})
	}
}

func TestLastOrStart(t *testing.T) {
	started := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	last := started.Add(time.Hour)

	if got := lastOrStart(last, started); !got.Equal(last) {
		t.Errorf("lastOrStart with a real poll = %v; want %v", got, last)
	}
	if got := lastOrStart(time.Time{}, started); !got.Equal(started) {
		t.Errorf("lastOrStart with no poll = %v; want process start %v", got, started)
	}
}

func TestStaleThreshold(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		// 60s × 10 = 10m, which equals the floor.
		{"default poll interval uses the floor", 60 * time.Second, 10 * time.Minute},
		{"short interval is floored", 5 * time.Second, 10 * time.Minute},
		{"long interval scales up", 5 * time.Minute, 50 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleThreshold(tc.interval); got != tc.want {
				t.Errorf("staleThreshold(%s) = %s; want %s", tc.interval, got, tc.want)
			}
		})
	}

	// The marker must never fire on a single missed poll, at any interval.
	for _, iv := range []time.Duration{time.Second, 30 * time.Second, time.Minute, 10 * time.Minute} {
		if got := staleThreshold(iv); got <= iv*2 {
			t.Errorf("staleThreshold(%s) = %s; too tight — a single blip would flag the board", iv, got)
		}
	}
}

func TestWatchdogCheckInterval(t *testing.T) {
	tests := []struct {
		limit time.Duration
		want  time.Duration
	}{
		{30 * time.Minute, 450 * time.Second}, // limit/4
		{time.Minute, time.Minute},            // floored
		{10 * time.Second, time.Minute},       // floored
	}
	for _, tc := range tests {
		if got := watchdogCheckInterval(tc.limit); got != tc.want {
			t.Errorf("watchdogCheckInterval(%s) = %s; want %s", tc.limit, got, tc.want)
		}
	}

	// A ticker interval must be strictly positive or time.NewTicker panics.
	for _, l := range []time.Duration{0, -time.Hour, time.Nanosecond} {
		if got := watchdogCheckInterval(l); got <= 0 {
			t.Errorf("watchdogCheckInterval(%s) = %s; must be positive", l, got)
		}
	}
}

func TestStaticRetryAt(t *testing.T) {
	now := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)

	// Daily refresh: a failure should be retried in an hour, not in a day, so the
	// next check tick (15 min apart) sees it due at now+1h.
	got := staticRetryAt(now, 24*time.Hour)
	if want := now.Add(staticRetryInterval - 24*time.Hour); !got.Equal(want) {
		t.Errorf("staticRetryAt(daily) = %v; want %v", got, want)
	}
	if due := got.Add(24 * time.Hour); !due.Equal(now.Add(staticRetryInterval)) {
		t.Errorf("next attempt due at %v; want %v", due, now.Add(staticRetryInterval))
	}

	// An interval already at or under the retry window has nothing to shorten,
	// and must never be pushed into the future (which would delay the retry).
	for _, iv := range []time.Duration{staticRetryInterval, 10 * time.Minute, time.Second} {
		if got := staticRetryAt(now, iv); !got.Equal(now) {
			t.Errorf("staticRetryAt(now, %s) = %v; want %v unchanged", iv, got, now)
		}
	}
}
