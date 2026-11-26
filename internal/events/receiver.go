package events

import (
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/NoahIsARider/nuncio/internal/state"
)

// Waker is the only thing a webhook is allowed to do.
type Waker interface {
	Wake(slug, source string) bool
}

// WebhookPlatform is a platform's verification material. Secret is the resolved
// value, never a reference; an empty Secret means unsigned deliveries are
// refused rather than trusted.
type WebhookPlatform struct {
	Kind      string
	Secret    string
	PublicKey ed25519.PublicKey
}

// Receiver accepts webhook deliveries, verifies them, discards the payload and
// wakes the scheduler. A 200 means "received", never "acted on".
type Receiver struct {
	Platforms map[string]WebhookPlatform
	Store     *state.Store
	Waker     Waker
	Now       func() time.Time
	Logf      func(format string, args ...any)
	MaxBody   int64
}

// NewReceiver builds a receiver. Missing optional fields get safe defaults.
func NewReceiver(platforms map[string]WebhookPlatform, store *state.Store, waker Waker) *Receiver {
	return &Receiver{
		Platforms: platforms, Store: store, Waker: waker,
		Now: time.Now, Logf: func(string, ...any) {},
		MaxBody: 2 << 20,
	}
}

// Handler returns the HTTP handler for /hooks/<platform>/<owner>/<name>.
func (r *Receiver) Handler() http.Handler {
	return http.HandlerFunc(r.serveHTTP)
}

func (r *Receiver) serveHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	platform, owner, name, ok := parseHookPath(req.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cfg, known := r.Platforms[platform]
	if !known {
		http.Error(w, "unknown platform", http.StatusNotFound)
		return
	}
	repo, err := r.Store.FindRepoByRemote(req.Context(), platform, owner, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unmanaged repository: accepted by routing, refused by policy. It is
			// a 404 so a misconfigured webhook is obvious, and no work happens.
			http.Error(w, "repository is not managed by nuncio", http.StatusNotFound)
			return
		}
		r.Logf("webhook: resolve %s/%s/%s: %v", platform, owner, name, err)
		http.Error(w, "state lookup failed", http.StatusInternalServerError)
		return
	}

	max := r.MaxBody
	if max <= 0 {
		max = 2 << 20
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, max+1))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > max {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	deliveryID := ""
	switch cfg.Kind {
	case "github":
		deliveryID = req.Header.Get("X-GitHub-Delivery")
		if err := VerifyGitHub(cfg.Secret, body, req.Header.Get("X-Hub-Signature-256")); err != nil {
			r.reject(w, platform, deliveryID, err)
			return
		}
	case "forgejo":
		deliveryID = firstHeader(req, "X-Forgejo-Delivery", "X-Gitea-Delivery")
		err := VerifyForgejo(cfg.Secret, body, firstHeader(req, "X-Forgejo-Signature", "X-Gitea-Signature"))
		if err != nil {
			r.reject(w, platform, deliveryID, err)
			return
		}
	case "gitlab":
		deliveryID = firstHeader(req, "X-Gitlab-Event-UUID", "Idempotency-Key")
		if err := VerifyGitLab(cfg.Secret, req.Header.Get("X-Gitlab-Token")); err != nil {
			r.reject(w, platform, deliveryID, err)
			return
		}
	case "sourcehut":
		nonce := req.Header.Get("X-Payload-Nonce")
		deliveryID = nonce
		err := VerifySourceHut(cfg.PublicKey, body, nonce, req.Header.Get("X-Payload-Signature"))
		if err != nil {
			r.reject(w, platform, deliveryID, err)
			return
		}
	default:
		r.Logf("webhook: %s has no verification scheme", platform)
		http.Error(w, "platform has no webhook scheme", http.StatusForbidden)
		return
	}

	if deliveryID == "" {
		sum := sha256.Sum256(body)
		deliveryID = "body:" + hex.EncodeToString(sum[:16])
	}
	now := r.Now().Unix()
	isNew, err := r.Store.MarkWebhookSeen(req.Context(), deliveryID, now)
	if err != nil {
		r.Logf("webhook: record delivery %s: %v", deliveryID, err)
		http.Error(w, "state write failed", http.StatusInternalServerError)
		return
	}
	if !isNew {
		// A retried delivery is harmless: the first one already woke the repo.
		fmt.Fprintln(w, "duplicate")
		return
	}
	r.Waker.Wake(repo.Slug, "webhook:"+platform)
	r.Logf("webhook: %s %s delivery %s woke %s", platform, owner+"/"+name, shortID(deliveryID), repo.Slug)
	fmt.Fprintln(w, "ok")
}

func (r *Receiver) reject(w http.ResponseWriter, platform, deliveryID string, err error) {
	switch {
	case errors.Is(err, ErrNoSecret):
		r.Logf("webhook: %s refused: %v", platform, err)
		http.Error(w, "webhook secret is not configured", http.StatusForbidden)
	case errors.Is(err, ErrMissingSignature), errors.Is(err, ErrBadSignature):
		r.Logf("webhook: %s refused delivery %s: %v", platform, shortID(deliveryID), err)
		http.Error(w, "invalid signature", http.StatusForbidden)
	default:
		r.Logf("webhook: %s refused delivery %s: %v", platform, shortID(deliveryID), err)
		http.Error(w, "invalid delivery", http.StatusForbidden)
	}
}

func parseHookPath(path string) (platform, owner, name string, ok bool) {
	const prefix = "/hooks/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", "", false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, prefix), "/"), "/")
	if len(parts) != 3 {
		return "", "", "", false
	}
	decode := func(s string) (string, bool) {
		v, err := url.PathUnescape(s)
		if err != nil || v == "" || strings.ContainsAny(v, "/\\") {
			return "", false
		}
		return v, true
	}
	if platform, ok = decode(parts[0]); !ok {
		return "", "", "", false
	}
	if owner, ok = decode(parts[1]); !ok {
		return "", "", "", false
	}
	if name, ok = decode(parts[2]); !ok {
		return "", "", "", false
	}
	return platform, owner, name, true
}

func firstHeader(req *http.Request, names ...string) string {
	for _, n := range names {
		if v := req.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	if id == "" {
		return "(none)"
	}
	return id
}
