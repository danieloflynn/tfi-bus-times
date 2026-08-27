package gtfs

import (
	"net"
	"net/http"
	"time"
)

// Every outbound call in this package must go through one of the clients below.
// Never use http.DefaultClient / http.Get / http.Head here: they have no
// Timeout, so a request whose socket dies without an RST blocks its goroutine
// forever. That is not hypothetical on this hardware — the Pi Zero 2W's onboard
// brcmfmac WiFi enters power save and silently drops established TCP
// connections (and the upstream AP/NAT can expire them too). The kernel keeps
// the socket open, the read never returns, and the poller goroutine wedges.
//
// The observable failure was: the board's "Updated: HH:MM:SS" header freezes at
// the moment of the hang, LiveStore stops being refreshed, and every arrival
// falls back to its scheduled time — a board that looks alive but is stuck in
// the past. Because a wedged goroutine is not a crashed process, systemd never
// restarted it either. Bounded timeouts are the fix; keep them.
const (
	// dialTimeout bounds connection setup (including a DNS lookup that never
	// answers, another way a dead WiFi link stalls a request).
	dialTimeout = 10 * time.Second
	// tlsHandshakeTimeout bounds the TLS handshake specifically, so a connection
	// that opens but never negotiates fails fast rather than eating the whole
	// request budget.
	tlsHandshakeTimeout = 10 * time.Second
	// responseHeaderTimeout bounds the wait for response headers after the
	// request is written — the phase a half-dead connection stalls in.
	responseHeaderTimeout = 20 * time.Second

	// idleConnTimeout is deliberately shorter than the default poll interval
	// (60 s) and than Go's 90 s default. After a WiFi drop a pooled keep-alive
	// connection is dead but still looks reusable, so the next poll would pick
	// it up and stall until its timeout. Expiring idle connections between polls
	// means each poll dials fresh and never inherits a corpse.
	idleConnTimeout = 30 * time.Second

	// rtTimeout is the whole-request budget for a realtime poll: dial through
	// final body byte. It must stay comfortably under the poll interval so a
	// slow poll can't outlive the tick that scheduled it.
	rtTimeout = 30 * time.Second

	// staticHeadTimeout bounds the cheap Last-Modified HEAD check.
	staticHeadTimeout = 30 * time.Second

	// staticDownloadTimeout is the whole-request budget for the GTFS static ZIP
	// (tens of MB). Generous, because a slow-but-progressing download on a
	// congested link is legitimate — but finite, so a stalled read can never
	// wedge the background refresher permanently.
	staticDownloadTimeout = 15 * time.Minute
)

// sharedTransport is used by every client in this package so connection pooling
// and the idle-connection expiry above are shared, not duplicated per client.
var sharedTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout: dialTimeout,
		// KeepAlive enables TCP keepalive probes, so a connection whose peer has
		// vanished is eventually torn down by the kernel instead of lingering.
		KeepAlive: 30 * time.Second,
	}).DialContext,
	TLSHandshakeTimeout:   tlsHandshakeTimeout,
	ResponseHeaderTimeout: responseHeaderTimeout,
	ExpectContinueTimeout: 1 * time.Second,
	IdleConnTimeout:       idleConnTimeout,
	MaxIdleConns:          4,
	MaxIdleConnsPerHost:   2,
	ForceAttemptHTTP2:     true,
}

var (
	// rtClient fetches the GTFS-RT TripUpdates feed every poll interval.
	rtClient = &http.Client{Transport: sharedTransport, Timeout: rtTimeout}
	// staticHeadClient performs the Last-Modified freshness check.
	staticHeadClient = &http.Client{Transport: sharedTransport, Timeout: staticHeadTimeout}
	// staticDownloadClient fetches the full GTFS static ZIP.
	staticDownloadClient = &http.Client{Transport: sharedTransport, Timeout: staticDownloadTimeout}
)
