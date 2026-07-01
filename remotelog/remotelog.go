// Package remotelog reports this device's own log lines to the dandev backend
// (POST /api/tfi/v1/activity_logs/report), so they're visible centrally
// instead of only on the device's serial console.
//
// It is a diagnostic sink, not part of any critical path: Log always returns
// immediately, sending the request from a background goroutine, and any
// network/auth/validation failure is logged locally (via the standard log
// package) and otherwise swallowed. A nil *Client is safe to call methods on
// (they no-op), so callers never need to nil-check before logging.
package remotelog

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Level is one of the four levels the API accepts.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// requestTimeout bounds each fire-and-forget POST so a stuck backend can never
// leak goroutines indefinitely.
const requestTimeout = 5 * time.Second

// rank orders levels for the minLevel filter. Unrecognised levels rank as
// LevelInfo so a bad config value degrades gracefully rather than silencing
// or flooding the log stream.
func rank(l Level) int {
	switch l {
	case LevelDebug:
		return 0
	case LevelInfo:
		return 1
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	default:
		return 1
	}
}

// ParseLevel maps a config string (e.g. "debug") to a Level, defaulting to
// LevelInfo for empty or unrecognised values.
func ParseLevel(s string) Level {
	switch Level(strings.ToLower(s)) {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return Level(strings.ToLower(s))
	default:
		return LevelInfo
	}
}

// Client posts activity log lines for one device. baseURL/deviceToken/minLevel
// are mutable via Update so a config reload can change them without
// rebuilding the client (and losing its pooled HTTP connections).
type Client struct {
	http *http.Client

	mu          sync.RWMutex
	baseURL     string
	deviceToken string
	minLevel    Level
}

// New builds a Client. baseURL/deviceToken may be empty — Log is then a no-op
// until a later Update supplies them.
func New(baseURL, deviceToken string, minLevel Level) *Client {
	c := &Client{http: &http.Client{Timeout: requestTimeout}}
	c.Update(baseURL, deviceToken, minLevel)
	return c
}

// Update swaps the target/credentials/filter, e.g. after config.yaml or
// secrets.yaml changes. A nil Client (e.g. a zero-value struct built without
// New, as in tests) is a safe no-op.
func (c *Client) Update(baseURL, deviceToken string, minLevel Level) {
	if c == nil {
		return
	}
	if minLevel == "" {
		minLevel = LevelInfo
	}
	c.mu.Lock()
	c.baseURL, c.deviceToken, c.minLevel = baseURL, deviceToken, minLevel
	c.mu.Unlock()
}

func (c *Client) settings() (baseURL, deviceToken string, minLevel Level) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL, c.deviceToken, c.minLevel
}

// Log reports one line at the given level. It never blocks the caller and
// never returns an error: a nil Client, an unconfigured baseURL/deviceToken,
// or a level below the configured minimum are all silent no-ops; a request
// that fails once sent is logged locally via the standard log package.
func (c *Client) Log(level Level, message string) {
	if c == nil {
		return
	}
	baseURL, deviceToken, minLevel := c.settings()
	if baseURL == "" || deviceToken == "" || rank(level) < rank(minLevel) {
		return
	}
	go c.send(baseURL, deviceToken, level, message)
}

// Debug/Info/Warn/Error are convenience wrappers around Log.
func (c *Client) Debug(message string) { c.Log(LevelDebug, message) }
func (c *Client) Info(message string)  { c.Log(LevelInfo, message) }
func (c *Client) Warn(message string)  { c.Log(LevelWarn, message) }
func (c *Client) Error(message string) { c.Log(LevelError, message) }

type reportBody struct {
	ActivityLog activityLog `json:"activity_log"`
}

type activityLog struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

func (c *Client) send(baseURL, deviceToken string, level Level, message string) {
	body, err := json.Marshal(reportBody{ActivityLog: activityLog{Level: string(level), Message: message}})
	if err != nil {
		log.Printf("remotelog: encoding %s log: %v", level, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/tfi/v1/activity_logs/report", bytes.NewReader(body))
	if err != nil {
		log.Printf("remotelog: building request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+deviceToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("remotelog: posting %s log: %v", level, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("remotelog: server returned %s for %s log", resp.Status, level)
	}
}
