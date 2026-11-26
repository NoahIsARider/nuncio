package events

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Runner reconciles one repository. It is supplied by the daemon; the scheduler
// never inspects the plan.
type Runner func(ctx context.Context, slug string) error

// RepoStatus is one repository's scheduling state, used by the status page.
type RepoStatus struct {
	Slug      string `json:"slug"`
	Running   bool   `json:"running"`
	Pending   bool   `json:"pending"`
	Runs      int64  `json:"runs"`
	LastRun   int64  `json:"last_run,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Source    string `json:"source,omitempty"`
}

type repoState struct {
	slug      string
	running   bool
	pending   bool
	runs      int64
	lastRun   time.Time
	lastError string
	source    string
}

// Scheduler coalesces wake-ups per repository, runs at most once per repository
// at a time, and bounds global concurrency. Every event source (webhook,
// firehose, timer, gate) goes through Wake; none of them acts directly.
type Scheduler struct {
	Concurrency int
	Interval    time.Duration
	Runner      Runner
	Logf        func(format string, args ...any)
	Now         func() time.Time

	mu      sync.Mutex
	repos   map[string]*repoState
	wakeCh  chan string
	sem     chan struct{}
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	wg      sync.WaitGroup
}

// NewScheduler returns a scheduler. Interval is the per-repository timer floor.
func NewScheduler(concurrency int, interval time.Duration, runner Runner) *Scheduler {
	if concurrency < 1 {
		concurrency = 1
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &Scheduler{
		Concurrency: concurrency,
		Interval:    interval,
		Runner:      runner,
		Logf:        func(string, ...any) {},
		Now:         time.Now,
		repos:       map[string]*repoState{},
		wakeCh:      make(chan string, 1024),
		done:        make(chan struct{}),
	}
}

// Register makes a repository eligible for timer reconciles.
func (s *Scheduler) Register(slug string) {
	if slug == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[slug]; !ok {
		s.repos[slug] = &repoState{slug: slug}
	}
}

// Wake asks for a reconcile. Repeated wakes while a run is in flight coalesce
// into one follow-up run. It never blocks and never runs the work inline.
func (s *Scheduler) Wake(slug, source string) bool {
	if slug == "" {
		return false
	}
	s.mu.Lock()
	st, ok := s.repos[slug]
	if !ok {
		st = &repoState{slug: slug}
		s.repos[slug] = st
	}
	st.pending = true
	if source != "" {
		st.source = source
	}
	s.mu.Unlock()
	select {
	case s.wakeCh <- slug:
	default:
		// The buffer is full, which means a lot of distinct repos were woken at
		// once. The timer will still pick this repo up; never block a webhook.
		s.Logf("scheduler: wake queue full, %s will be picked up by the timer", slug)
	}
	return true
}

// Start launches the dispatcher. It is idempotent.
func (s *Scheduler) Start(parent context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.sem = make(chan struct{}, s.Concurrency)
	s.started = true
	s.mu.Unlock()
	go s.loop()
}

// Stop cancels the dispatcher and waits for in-flight runs.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	cancel := s.cancel
	done := s.done
	s.started = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-done
	s.wg.Wait()
}

// Wait blocks until every in-flight run finishes. It does not stop the timer.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Status returns a deterministic snapshot for the status page.
func (s *Scheduler) Status() []RepoStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RepoStatus, 0, len(s.repos))
	for _, st := range s.repos {
		var last int64
		if !st.lastRun.IsZero() {
			last = st.lastRun.Unix()
		}
		out = append(out, RepoStatus{
			Slug: st.slug, Running: st.running, Pending: st.pending, Runs: st.runs,
			LastRun: last, LastError: st.lastError, Source: st.source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

func (s *Scheduler) loop() {
	defer close(s.done)
	tick := s.Interval
	if tick > time.Second {
		tick = time.Second
	}
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case slug := <-s.wakeCh:
			s.dispatch(slug)
		case <-ticker.C:
			s.tickDue()
		}
	}
}

func (s *Scheduler) tickDue() {
	now := s.Now()
	var due []string
	s.mu.Lock()
	for slug, st := range s.repos {
		if st.running {
			continue
		}
		if st.lastRun.IsZero() || now.Sub(st.lastRun) >= s.Interval {
			due = append(due, slug)
		}
	}
	s.mu.Unlock()
	sort.Strings(due)
	for _, slug := range due {
		s.dispatch(slug)
	}
}

func (s *Scheduler) dispatch(slug string) {
	s.mu.Lock()
	st, ok := s.repos[slug]
	if !ok {
		st = &repoState{slug: slug}
		s.repos[slug] = st
	}
	if st.running {
		st.pending = true
		s.mu.Unlock()
		return
	}
	st.running = true
	st.pending = false
	source := st.source
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-s.ctx.Done():
			s.finish(slug, context.Canceled)
			return
		}
		s.Logf("reconcile %s (%s)", slug, source)
		err := s.Runner(s.ctx, slug)
		s.finish(slug, err)
	}()
}

func (s *Scheduler) finish(slug string, err error) {
	s.mu.Lock()
	st := s.repos[slug]
	st.running = false
	st.runs++
	st.lastRun = s.Now()
	if err != nil && err != context.Canceled {
		st.lastError = err.Error()
		s.Logf("reconcile %s failed: %v", slug, err)
	} else {
		st.lastError = ""
	}
	pending := st.pending
	s.mu.Unlock()
	if pending {
		s.dispatch(slug)
	}
}
