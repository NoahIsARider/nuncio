package events

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// errCursorLoss means the knot no longer has the frame we asked for. The caller
// must fall back to a timer reconcile rather than replay a hole.
var errCursorLoss = errors.New("firehose cursor is no longer available")

// Firehose subscribes to a Tangled knot's com.atproto.sync.subscribeRepos
// stream. It filters #commit frames by repository DID so a busy public firehose
// never causes work for strangers' repositories.
type Firehose struct {
	URL          string
	RepoDIDs     map[string]string // repo DID -> logical slug
	OnCommit     func(slug string)
	OnCursorLoss func()
	Dialer       *websocket.Dialer
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	Logf         func(format string, args ...any)

	mu     sync.Mutex
	cursor int64
}

// NewFirehose builds a subscriber. repoNames maps a repo DID (for example
// did:plc:abc) to the managed slug.
func NewFirehose(rawURL string, repoNames map[string]string, onCommit func(string)) *Firehose {
	return &Firehose{
		URL: rawURL, RepoDIDs: repoNames, OnCommit: onCommit,
		Dialer:     websocket.DefaultDialer,
		MinBackoff: time.Second,
		MaxBackoff: 30 * time.Second,
		Logf:       func(string, ...any) {},
	}
}

// SetCursor persists the last sequence number so a reconnect resumes.
func (f *Firehose) SetCursor(v int64) {
	f.mu.Lock()
	f.cursor = v
	f.mu.Unlock()
}

// Cursor returns the last sequence number.
func (f *Firehose) Cursor() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor
}

// Run reconnects forever with exponential backoff until ctx is cancelled.
func (f *Firehose) Run(ctx context.Context) error {
	backoff := f.MinBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	max := f.MaxBackoff
	if max <= 0 {
		max = 30 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progressed, err := f.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errCursorLoss) {
			f.Logf("firehose: cursor lost, falling back to the timer reconcile")
			backoff = f.MinBackoff
			if backoff <= 0 {
				backoff = time.Second
			}
			continue
		}
		if progressed {
			backoff = f.MinBackoff
			if backoff <= 0 {
				backoff = time.Second
			}
		}
		if err != nil {
			f.Logf("firehose: %v; reconnecting in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > max {
			backoff = max
		}
	}
}

func (f *Firehose) runOnce(ctx context.Context) (bool, error) {
	u, err := url.Parse(f.URL)
	if err != nil {
		return false, fmt.Errorf("firehose URL: %w", err)
	}
	q := u.Query()
	if c := f.Cursor(); c > 0 {
		q.Set("cursor", strconv.FormatInt(c, 10))
	} else {
		q.Del("cursor")
	}
	u.RawQuery = q.Encode()

	dialer := f.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("dial %s: HTTP %d: %w", u.Host, resp.StatusCode, err)
		}
		return false, fmt.Errorf("dial %s: %w", u.Host, err)
	}
	defer conn.Close()
	conn.SetReadLimit(8 << 20)

	// ReadMessage is blocking; closing the connection is the only portable way
	// to interrupt it when the daemon is shutting down.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	progressed := false
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return progressed, ctx.Err()
			}
			return progressed, err
		}
		header, body, err := ParseFirehoseFrame(data)
		if err != nil {
			f.Logf("firehose: drop malformed frame: %v", err)
			continue
		}
		progressed = true
		kind, _ := header["t"].(string)
		switch kind {
		case "#commit":
			f.advance(body)
			did, _ := body["repo"].(string)
			if slug, ok := f.RepoDIDs[did]; ok && f.OnCommit != nil {
				f.OnCommit(slug)
			}
		case "#error":
			msg, _ := body["error"].(string)
			if strings.Contains(msg, "FutureCursor") || strings.Contains(msg, "ConsumerTooSlow") {
				f.SetCursor(0)
				if f.OnCursorLoss != nil {
					f.OnCursorLoss()
				}
				return progressed, errCursorLoss
			}
			return progressed, fmt.Errorf("firehose error frame: %s", msg)
		default:
			// #identity, #account, #handle, #tombstone and future frames carry
			// sequence numbers but say nothing about refs.
			f.advance(body)
		}
	}
}

func (f *Firehose) advance(body map[string]any) {
	if seq := asInt64(body["seq"]); seq > 0 {
		f.SetCursor(seq)
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		x, _ := strconv.ParseInt(n, 10, 64)
		return x
	default:
		return 0
	}
}
