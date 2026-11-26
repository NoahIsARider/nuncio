package events

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NoahIsARider/nuncio/internal/state"
)

// ------------------------------------------------------------------ CBOR

type cborBuf struct{ bytes.Buffer }

func (b *cborBuf) head(major byte, v uint64) {
	if v < 24 {
		b.WriteByte(major<<5 | byte(v))
		return
	}
	if v < 256 {
		b.WriteByte(major<<5 | 24)
		b.WriteByte(byte(v))
		return
	}
	b.WriteByte(major<<5 | 25)
	b.WriteByte(byte(v >> 8))
	b.WriteByte(byte(v))
}

func (b *cborBuf) text(s string) { b.head(3, uint64(len(s))); b.WriteString(s) }
func (b *cborBuf) uint(v uint64) { b.head(0, v) }
func (b *cborBuf) mapHead(n int) { b.head(5, uint64(n)) }

func commitFrame(seq uint64, did string) []byte {
	var b cborBuf
	b.mapHead(2)
	b.text("op")
	b.uint(1)
	b.text("t")
	b.text("#commit")
	b.mapHead(2)
	b.text("seq")
	b.uint(seq)
	b.text("repo")
	b.text(did)
	return b.Bytes()
}

func errorFrame(msg string) []byte {
	var b cborBuf
	b.mapHead(1)
	b.text("t")
	b.text("#error")
	b.mapHead(1)
	b.text("error")
	b.text(msg)
	return b.Bytes()
}

func TestParseFirehoseFrame(t *testing.T) {
	header, body, err := ParseFirehoseFrame(commitFrame(42, "did:plc:abc"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if header["t"] != "#commit" {
		t.Fatalf("header t = %v", header["t"])
	}
	if asInt64(body["seq"]) != 42 {
		t.Fatalf("seq = %v", body["seq"])
	}
	if body["repo"] != "did:plc:abc" {
		t.Fatalf("repo = %v", body["repo"])
	}
}

func TestDecodeCBORRejectsTruncated(t *testing.T) {
	if _, _, err := DecodeCBOR([]byte{0x62, 0x61}); err == nil {
		t.Fatal("expected an error for a truncated string")
	}
}

// -------------------------------------------------------------- signatures

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHub(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	secret := "hunter2"
	good := "sha256=" + hmacHex(secret, body)
	if err := VerifyGitHub(secret, body, good); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifyGitHub(secret, body, "sha256="+strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong signature accepted")
	}
	if err := VerifyGitHub(secret, []byte("tampered"), good); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := VerifyGitHub("", body, good); err != ErrNoSecret {
		t.Fatalf("empty secret: got %v", err)
	}
	if err := VerifyGitHub(secret, body, ""); err != ErrMissingSignature {
		t.Fatalf("missing header: got %v", err)
	}
}

func TestVerifyForgejo(t *testing.T) {
	body := []byte("payload")
	secret := "forgejo"
	want := hmacHex(secret, body)
	if err := VerifyForgejo(secret, body, want); err != nil {
		t.Fatalf("plain hex: %v", err)
	}
	if err := VerifyForgejo(secret, body, "sha256="+want); err != nil {
		t.Fatalf("prefixed hex: %v", err)
	}
	if err := VerifyForgejo(secret, body, strings.Repeat("a", 64)); err == nil {
		t.Fatal("wrong signature accepted")
	}
}

func TestVerifyGitLab(t *testing.T) {
	if err := VerifyGitLab("tok", "tok"); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if err := VerifyGitLab("tok", "other"); err == nil {
		t.Fatal("wrong token accepted")
	}
	if err := VerifyGitLab("tok", ""); err != ErrMissingSignature {
		t.Fatalf("missing token: %v", err)
	}
}

func TestVerifySourceHut(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"query":"..."}`)
	nonce := "n-123"
	sig := SignSourceHut(priv, body, nonce)
	if err := VerifySourceHut(pub, body, nonce, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySourceHut(pub, body, "n-124", sig); err == nil {
		t.Fatal("wrong nonce accepted")
	}
	if err := VerifySourceHut(pub, body, nonce, ""); err == nil {
		t.Fatal("missing signature accepted")
	}
	parsed, err := ParseEd25519PublicKey(base64.StdEncoding.EncodeToString(pub))
	if err != nil || !bytes.Equal(parsed, pub) {
		t.Fatalf("parse base64 key: %v", err)
	}
	if _, err := ParseEd25519PublicKey("not-a-key"); err == nil {
		t.Fatal("bad key accepted")
	}
}

// ---------------------------------------------------------------- receiver

type fakeWaker struct {
	mu    sync.Mutex
	woken []string
}

func (f *fakeWaker) Wake(slug, source string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.woken = append(f.woken, slug+"|"+source)
	return true
}

func (f *fakeWaker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.woken)
}

func newReceiverTest(t *testing.T) (*Receiver, *fakeWaker, func(string) *http.Request) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	repo, err := st.EnsureRepo(ctx, "alice/example", "default", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRepoPlatform(ctx, state.RepoPlatform{
		RepoID: repo.ID, Platform: "github", Owner: "alice", Name: "example",
	}); err != nil {
		t.Fatal(err)
	}
	waker := &fakeWaker{}
	r := NewReceiver(map[string]WebhookPlatform{
		"github": {Kind: "github", Secret: "hunter2"},
	}, st, waker)
	r.Now = func() time.Time { return time.Unix(1000, 0) }

	makeReq := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/hooks/github/alice/example", strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex("hunter2", []byte(body)))
		req.Header.Set("X-GitHub-Delivery", "delivery-"+fmt.Sprint(len(body)))
		return req
	}
	return r, waker, makeReq
}

func TestReceiverVerifiesAndWakes(t *testing.T) {
	r, waker, makeReq := newReceiverTest(t)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, makeReq(`{"ref":"refs/heads/main"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if waker.count() != 1 {
		t.Fatalf("wakes = %d", waker.count())
	}
}

func TestReceiverRejectsBadSignature(t *testing.T) {
	r, waker, makeReq := newReceiverTest(t)
	req := makeReq(`{"ref":"refs/heads/main"}`)
	req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if waker.count() != 0 {
		t.Fatal("a rejected delivery woke the scheduler")
	}
}

func TestReceiverDedupesDelivery(t *testing.T) {
	r, waker, makeReq := newReceiverTest(t)
	body := `{"ref":"refs/heads/main"}`
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, makeReq(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if waker.count() != 1 {
		t.Fatalf("duplicate delivery woke the scheduler %d times", waker.count())
	}
}

func TestReceiverUnknownRepoIs404(t *testing.T) {
	r, waker, _ := newReceiverTest(t)
	req := httptest.NewRequest(http.MethodPost, "/hooks/github/alice/other", strings.NewReader("{}"))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex("hunter2", []byte("{}")))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	if waker.count() != 0 {
		t.Fatal("unmanaged repo woke the scheduler")
	}
}

func TestReceiverRefusesUnsignedWhenNoSecret(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	repo, _ := st.EnsureRepo(ctx, "a/b", "default", 1)
	st.SetRepoPlatform(ctx, state.RepoPlatform{RepoID: repo.ID, Platform: "github", Owner: "a", Name: "b"})
	r := NewReceiver(map[string]WebhookPlatform{"github": {Kind: "github"}}, st, &fakeWaker{})
	req := httptest.NewRequest(http.MethodPost, "/hooks/github/a/b", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
}

// --------------------------------------------------------------- scheduler

func TestSchedulerCoalescesAndSerializes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var runs int32
	var active int32
	var maxActive int32
	s := NewScheduler(2, time.Hour, func(ctx context.Context, slug string) error {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if n <= old || atomic.CompareAndSwapInt32(&maxActive, old, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		atomic.AddInt32(&runs, 1)
		return nil
	})
	s.Start(context.Background())
	defer s.Stop()

	for i := 0; i < 3; i++ {
		s.Wake("alice/example", "test")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	deadline := time.After(3 * time.Second)
	for atomic.LoadInt32(&runs) < 2 {
		select {
		case <-deadline:
			t.Fatalf("runs = %d, want 2", atomic.LoadInt32(&runs))
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&runs); got != 2 {
		t.Fatalf("runs = %d, want exactly 2 (three wakes must coalesce into one follow-up)", got)
	}
	if atomic.LoadInt32(&maxActive) > 2 {
		t.Fatalf("max concurrency = %d", atomic.LoadInt32(&maxActive))
	}
}

func TestSchedulerConcurrencyBound(t *testing.T) {
	var active, maxActive int32
	s := NewScheduler(2, time.Hour, func(ctx context.Context, slug string) error {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if n <= old || atomic.CompareAndSwapInt32(&maxActive, old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil
	})
	s.Start(context.Background())
	defer s.Stop()
	for _, slug := range []string{"a/1", "a/2", "a/3", "a/4", "a/5", "a/6"} {
		s.Wake(slug, "test")
	}
	s.Wait()
	if got := atomic.LoadInt32(&maxActive); got > 2 {
		t.Fatalf("max concurrency = %d, want <= 2", got)
	}
	if got := len(s.Status()); got != 6 {
		t.Fatalf("status entries = %d", got)
	}
}

func TestSchedulerTimer(t *testing.T) {
	var runs int32
	s := NewScheduler(1, 20*time.Millisecond, func(ctx context.Context, slug string) error {
		atomic.AddInt32(&runs, 1)
		return nil
	})
	s.Register("a/b")
	s.Start(context.Background())
	time.Sleep(120 * time.Millisecond)
	s.Stop()
	if atomic.LoadInt32(&runs) < 2 {
		t.Fatalf("timer ran %d times, want at least 2", runs)
	}
}

func TestSchedulerFiftyReposConcurrency(t *testing.T) {
	const repos = 50
	var active, maxActive int32
	var mu sync.Mutex
	runs := map[string]int{}
	done := make(chan struct{}, repos)
	s := NewScheduler(4, time.Hour, func(ctx context.Context, slug string) error {
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if n <= old || atomic.CompareAndSwapInt32(&maxActive, old, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		mu.Lock()
		runs[slug]++
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	s.Start(context.Background())
	defer s.Stop()
	for i := 0; i < repos; i++ {
		slug := fmt.Sprintf("alice/r%02d", i)
		s.Register(slug)
		s.Wake(slug, "scale")
	}
	for i := 0; i < repos; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d repos ran", i, repos)
		}
	}
	s.Wait()
	if got := atomic.LoadInt32(&maxActive); got > 4 {
		t.Fatalf("max concurrency = %d, want <= 4", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != repos {
		t.Fatalf("reconciled %d repos, want %d", len(runs), repos)
	}
	for slug, n := range runs {
		if n != 1 {
			t.Fatalf("%s ran %d times, want exactly 1", slug, n)
		}
	}
}

// ---------------------------------------------------------------- firehose

func TestFirehoseFiltersCommitsAndResumes(t *testing.T) {
	cursors := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursors <- r.URL.Query().Get("cursor")
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.BinaryMessage, commitFrame(7, "did:plc:managed")); err != nil {
			return
		}
		// A frame for a stranger's repository must not wake anything.
		_ = conn.WriteMessage(websocket.BinaryMessage, commitFrame(8, "did:plc:stranger"))
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	received := make(chan string, 4)
	f := NewFirehose("ws"+strings.TrimPrefix(srv.URL, "http")+"/xrpc/com.atproto.sync.subscribeRepos",
		map[string]string{"did:plc:managed": "alice/example"},
		func(slug string) { received <- slug })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = f.Run(ctx) }()

	select {
	case slug := <-received:
		if slug != "alice/example" {
			t.Fatalf("slug = %q", slug)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no commit delivered")
	}
	deadline := time.After(2 * time.Second)
	for f.Cursor() != 8 {
		select {
		case <-deadline:
			t.Fatalf("cursor = %d, want 8", f.Cursor())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
}

func TestFirehoseCursorLossFallsBack(t *testing.T) {
	var connections int32
	secondCursor := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := atomic.AddInt32(&connections, 1)
		if n == 1 {
			_ = conn.WriteMessage(websocket.BinaryMessage, errorFrame("FutureCursor"))
			time.Sleep(20 * time.Millisecond)
			return
		}
		secondCursor <- r.URL.Query().Get("cursor")
		_ = conn.WriteMessage(websocket.BinaryMessage, commitFrame(99, "did:plc:m"))
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	lost := make(chan struct{}, 1)
	received := make(chan string, 1)
	f := NewFirehose("ws"+strings.TrimPrefix(srv.URL, "http")+"/sub",
		map[string]string{"did:plc:m": "a/b"}, func(string) { received <- "ok" })
	f.MinBackoff = 10 * time.Millisecond
	f.MaxBackoff = 50 * time.Millisecond
	f.OnCursorLoss = func() { lost <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = f.Run(ctx) }()

	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("cursor loss was not reported")
	}
	select {
	case c := <-secondCursor:
		if c != "" {
			t.Fatalf("second connection sent cursor %q, want empty", c)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("firehose did not reconnect")
	}
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("no commit after recovery")
	}
	cancel()
}

// -------------------------------------------------------------------- gate

func TestGateWarnsButNeverBlocks(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	repo, _ := st.EnsureRepo(ctx, "a/b", "default", 1)
	st.SetRepoPlatform(ctx, state.RepoPlatform{RepoID: repo.ID, Platform: "srht", Owner: "a", Name: "b"})

	g := GateFromStore(st, func(context.Context, string, string) (map[string]string, error) {
		return map[string]string{"github": "0123456789abcdef", "codeberg": "0123456789abcdef"}, nil
	})
	msg, blocked := g.Check(ctx, "a/b", GateEvent{Ref: "refs/heads/main", OldSHA: "aaaa", NewSHA: "bbbb"})
	if blocked {
		t.Fatal("gate blocked a push; it is warn-only until S1 is verified")
	}
	if !strings.Contains(msg, "github 0123456") || !strings.Contains(msg, "NOT blocked") {
		t.Fatalf("message does not name the peer and the non-blocking status: %q", msg)
	}
}
