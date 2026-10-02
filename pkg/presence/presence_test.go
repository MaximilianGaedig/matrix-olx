package presence

import (
	"context"
	"fmt"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
)

type sentReq struct {
	key string
	p   event.Presence
}

type harness struct {
	m    *Manager
	now  time.Time
	sent []sentReq
	fail bool
}

func newHarness(cfg Config) *harness {
	h := &harness{now: time.Unix(1_700_000_000, 0)}
	h.m = NewManager(cfg, func(ctx context.Context, key string, p event.Presence) error {
		if h.fail {
			return fmt.Errorf("boom")
		}
		h.sent = append(h.sent, sentReq{key: key, p: p})
		return nil
	})
	h.m.now = func() time.Time { return h.now }
	h.m.limiter = NewTokenBucket(h.m.cfg.RatePerSecond, h.m.cfg.Burst, h.now)
	return h
}

func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.m.Tick(context.Background())
}

func (h *harness) take() []sentReq {
	s := h.sent
	h.sent = nil
	return s
}

func TestOnlineSentImmediatelyAndRefreshed(t *testing.T) {
	h := newHarness(Config{Debounce: 10 * time.Second, Refresh: 4 * time.Minute})
	h.m.Update("a", State{Presence: event.PresenceOnline})
	h.advance(0)
	if s := h.take(); len(s) != 1 || s[0].p != event.PresenceOnline {
		t.Fatalf("expected one online send, got %v", s)
	}
	// Repeated confirmations don't cause requests before the refresh interval.
	for i := 0; i < 30; i++ {
		h.m.Update("a", State{Presence: event.PresenceOnline})
		h.advance(10 * time.Second)
	}
	if s := h.take(); len(s) != 1 || s[0].p != event.PresenceOnline {
		t.Fatalf("expected exactly one refresh within 300s, got %v", s)
	}
}

func TestDebounceCoalescesFlapping(t *testing.T) {
	h := newHarness(Config{Debounce: 10 * time.Second})
	h.m.Update("a", State{Presence: event.PresenceOnline})
	h.advance(0)
	h.take()
	for i := 0; i < 5; i++ {
		h.m.Update("a", State{Presence: event.PresenceOffline})
		h.advance(time.Second)
		h.m.Update("a", State{Presence: event.PresenceOnline})
		h.advance(time.Second)
	}
	h.m.Update("a", State{Presence: event.PresenceOffline})
	h.advance(time.Second)
	// 11s passed since the online send, so exactly one offline goes out.
	if s := h.take(); len(s) != 1 || s[0].p != event.PresenceOffline {
		t.Fatalf("expected a single coalesced offline, got %v", s)
	}
}

func TestNewOfflineUsersAreNotSent(t *testing.T) {
	h := newHarness(Config{})
	for i := 0; i < 100; i++ {
		h.m.Update(fmt.Sprint(i), State{Presence: event.PresenceOffline})
	}
	h.advance(time.Minute)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("expected no sends for initially-offline users, got %d", len(s))
	}
}

func TestExpiryDecaysOnline(t *testing.T) {
	h := newHarness(Config{Debounce: time.Second, Refresh: 4 * time.Minute})
	h.m.Update("a", State{Presence: event.PresenceOnline, Until: h.now.Add(90 * time.Second)})
	h.advance(0)
	h.take()
	h.advance(89 * time.Second)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("unexpected send before expiry: %v", s)
	}
	h.advance(time.Second)
	if s := h.take(); len(s) != 1 || s[0].p != event.PresenceOffline {
		t.Fatalf("expected offline after expiry, got %v", s)
	}
	// And no online refresh afterwards.
	h.advance(10 * time.Minute)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("unexpected send after expiry: %v", s)
	}
}

func TestGlobalRateLimit(t *testing.T) {
	h := newHarness(Config{RatePerSecond: 2, Burst: 5})
	for i := 0; i < 50; i++ {
		h.m.Update(fmt.Sprint(i), State{Presence: event.PresenceOnline})
	}
	h.advance(0)
	if n := len(h.take()); n != 5 {
		t.Fatalf("expected burst of 5, got %d", n)
	}
	h.advance(time.Second)
	if n := len(h.take()); n != 2 {
		t.Fatalf("expected 2 after one second, got %d", n)
	}
	total := 7
	for i := 0; i < 30 && total < 50; i++ {
		h.advance(time.Second)
		total += len(h.take())
	}
	if total != 50 {
		t.Fatalf("expected all 50 eventually, got %d", total)
	}
}

func TestErrorBacksOffAndRetries(t *testing.T) {
	h := newHarness(Config{Debounce: 10 * time.Second})
	h.fail = true
	h.m.Update("a", State{Presence: event.PresenceOnline})
	h.advance(0)
	h.fail = false
	h.advance(5 * time.Second)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("expected backoff, got %v", s)
	}
	h.advance(5 * time.Second)
	if s := h.take(); len(s) != 1 {
		t.Fatalf("expected retry after debounce, got %v", s)
	}
}

func TestMaxTrackedEvictsSettled(t *testing.T) {
	h := newHarness(Config{MaxTracked: 3})
	h.m.Update("off1", State{Presence: event.PresenceOffline})
	h.m.Update("on1", State{Presence: event.PresenceOnline})
	h.m.Update("on2", State{Presence: event.PresenceOnline})
	h.m.Update("on3", State{Presence: event.PresenceOnline})
	if _, ok := h.m.entries["off1"]; ok {
		t.Fatal("settled offline entry should have been evicted")
	}
	h.m.Update("on4", State{Presence: event.PresenceOnline})
	if _, ok := h.m.entries["on4"]; ok {
		t.Fatal("online entries must not be evicted")
	}
}

func TestTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	tb := NewTokenBucket(1, 2, now)
	// One call per token, in order: the repetition is the point, since each Allow spends one. Written
	// as a loop rather than a chain of ||, because staticcheck reads identical calls as a copy-paste
	// slip (SA4000) and cannot know this one has side effects.
	for i, want := range []bool{true, true, false} {
		if tb.Allow(now) != want {
			t.Fatalf("burst of 2 expected (call %d)", i)
		}
	}
	if tb.Allow(now.Add(500 * time.Millisecond)) {
		t.Fatal("half a token should not be enough")
	}
	if !tb.Allow(now.Add(time.Second)) {
		t.Fatal("one token after a second")
	}
	for i, want := range []bool{true, true, false} {
		if tb.Allow(now.Add(time.Hour)) != want {
			t.Fatalf("refill must cap at burst (call %d)", i)
		}
	}
}

func TestOnlyPresenceIsSent(t *testing.T) {
	h := newHarness(Config{})
	// First sight of anyone not online isn't sent (it would stamp them active now).
	h.m.Update("hidden", State{Presence: event.PresenceUnavailable})
	h.m.Update("exact", State{Presence: event.PresenceOffline})
	h.m.Update("online", State{Presence: event.PresenceOnline})
	h.advance(time.Minute)
	s := h.take()
	if len(s) != 1 || s[0].key != "online" || s[0].p != event.PresenceOnline {
		t.Fatalf("unexpected sends: %+v", s)
	}
	// Going offline is sent once, as offline.
	h.m.Update("online", State{Presence: event.PresenceUnavailable})
	h.advance(time.Minute)
	if s = h.take(); len(s) != 1 || s[0].p != event.PresenceOffline {
		t.Fatalf("expected one offline, got %+v", s)
	}
	h.m.Update("online", State{Presence: event.PresenceOffline})
	h.advance(time.Minute)
	if s = h.take(); len(s) != 0 {
		t.Fatalf("offline re-sent: %+v", s)
	}
}

func TestActivityMakesOnlineForAWhile(t *testing.T) {
	h := newHarness(Config{Debounce: time.Second})
	h.m.Activity("u", h.now)
	h.advance(0)
	if s := h.take(); len(s) != 1 || s[0].p != event.PresenceOnline {
		t.Fatalf("expected online, got %+v", s)
	}
	h.advance(ActivityOnline)
	if s := h.take(); len(s) != 1 || s[0].p != event.PresenceOffline {
		t.Fatalf("expected offline after the activity window, got %+v", s)
	}
	// Old activity (backfill) says nothing about now.
	h.m.Activity("old", h.now.Add(-time.Hour))
	h.advance(time.Minute)
	if s := h.take(); len(s) != 0 {
		t.Fatalf("old activity sent: %+v", s)
	}
}

// Messenger repeats a user's last-active time in every snapshot; the homeserver wants it once.
func TestSeenReporterIsNews(t *testing.T) {
	r := NewSeenReporter(nil)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	if !r.isNews("1", at, now) {
		t.Fatal("a first last-active time is news")
	}
	if r.isNews("1", at, now) || r.isNews("1", at.Add(300*time.Millisecond), now) {
		t.Error("the same time again is not")
	}
	if r.isNews("1", at.Add(-time.Minute), now) {
		t.Error("an older time is not")
	}
	if !r.isNews("1", at.Add(time.Minute), now) {
		t.Error("a later time is")
	}
	if !r.isNews("2", at, now) {
		t.Error("another user's time is their own")
	}
	if r.isNews("3", time.Time{}, now) || r.isNews("3", now.Add(time.Hour), now) || r.isNews("", at, now) {
		t.Error("no time, a time that hasn't come and nobody are not news")
	}
}

func TestSeenReporterStopsWhenUnsupported(t *testing.T) {
	calls := 0
	r := NewSeenReporter(func(context.Context, string, time.Time) error {
		calls++
		return ErrSeenUnsupported
	})
	r.Note("1", time.Now().Add(-time.Hour))
	r.Run(context.Background())
	r.Note("2", time.Now().Add(-time.Hour))
	if calls != 1 || len(r.queue) != 0 {
		t.Errorf("asked %d times, %d still queued", calls, len(r.queue))
	}
}
