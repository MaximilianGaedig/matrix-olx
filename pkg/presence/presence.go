// Package presence bridges remote online status to Matrix presence on ghost
// users.
//
// Remote networks report status changes at their own pace, while the Matrix
// homeserver decays presence on its own: tuwunel (and Synapse) turn "online"
// into "unavailable" after presence_idle_timeout_s (300s by default) unless it
// is asserted again. The Manager therefore keeps the latest desired state per
// remote user and a single worker loop that
//
//   - coalesces bursts of updates for one user (per-ghost debounce),
//   - re-asserts "online" periodically while the remote still reports the user
//     as online (Refresh, which must be below the homeserver idle timeout),
//   - lets an online state expire when the remote gave (or the bridge assumed)
//     an expiry and nothing confirmed it since, and
//   - caps the total request rate to the homeserver with a token bucket.
package presence

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
)

// SendFunc sets the presence of the ghost belonging to the given remote user.
// It takes no status message on purpose: bridges send presence only, and a
// "last seen" text would be a second, staler source next to the homeserver's
// own last_active_ago.
type SendFunc func(ctx context.Context, remoteUserID string, presence event.Presence) error

// State is a remote user's status as reported by the remote network.
type State struct {
	Presence event.Presence
	// Until is when an online state stops being valid unless the remote
	// confirms it again. Zero means "until the remote says otherwise".
	Until time.Time
}

type Config struct {
	// Debounce is the minimum time between two requests for the same ghost.
	Debounce time.Duration
	// Refresh is how often an online state is re-sent to keep the homeserver
	// from decaying it to unavailable. Must be below the homeserver's idle
	// timeout (tuwunel: presence_idle_timeout_s = 300).
	Refresh time.Duration
	// RatePerSecond and Burst form the global token bucket for all requests.
	RatePerSecond float64
	Burst         int
	// MaxTracked caps the number of remote users kept in memory.
	MaxTracked int
	// TickInterval is how often the worker loop runs.
	TickInterval time.Duration
}

func (c *Config) setDefaults() {
	if c.Debounce <= 0 {
		c.Debounce = 10 * time.Second
	}
	if c.Refresh <= 0 {
		c.Refresh = 4 * time.Minute
	}
	if c.RatePerSecond <= 0 {
		c.RatePerSecond = 5
	}
	if c.Burst <= 0 {
		c.Burst = 10
	}
	if c.MaxTracked <= 0 {
		c.MaxTracked = 10000
	}
	if c.TickInterval <= 0 {
		c.TickInterval = time.Second
	}
}

type entry struct {
	desired event.Presence
	until   time.Time
	sent    event.Presence
	sentAt  time.Time
	touched time.Time
}

// Manager tracks desired presence per remote user and sends it to Matrix.
type Manager struct {
	cfg     Config
	send    SendFunc
	now     func() time.Time
	limiter *TokenBucket

	lock    sync.Mutex
	entries map[string]*entry
}

func NewManager(cfg Config, send SendFunc) *Manager {
	cfg.setDefaults()
	return &Manager{
		cfg:     cfg,
		send:    send,
		now:     time.Now,
		limiter: NewTokenBucket(cfg.RatePerSecond, cfg.Burst, time.Now()),
		entries: make(map[string]*entry),
	}
}

// Update records the latest remote status of a user. It never blocks on the
// homeserver; the worker loop sends the change later.
func (m *Manager) Update(remoteUserID string, st State) {
	if m == nil || remoteUserID == "" || st.Presence == "" {
		return
	}
	now := m.now()
	m.lock.Lock()
	defer m.lock.Unlock()
	// Only presence goes to Matrix, never a status message: the homeserver's last_active_ago is the
	// one "last seen", and it's stamped with every presence the bridge sets. So an offline is only
	// sent when we saw the user go offline (the moment they were last active), never re-sent, and
	// never sent for someone we first see offline (that would say they were active just now).
	if st.Presence == event.PresenceUnavailable {
		st.Presence = event.PresenceOffline
	}
	e, ok := m.entries[remoteUserID]
	if !ok {
		if len(m.entries) >= m.cfg.MaxTracked && !m.evictOne() {
			return
		}
		e = &entry{}
		m.entries[remoteUserID] = e
		if st.Presence != event.PresenceOnline {
			e.sent = st.Presence
			e.sentAt = now
		}
	}
	e.desired = st.Presence
	e.until = st.Until
	e.touched = now
}

// ActivityOnline is how long someone counts as online after we saw them do something.
const ActivityOnline = 5 * time.Minute

// Activity records that a remote user did something at `at` (sent a message, typed, read ours): they
// are online until ActivityOnline after it, which also covers networks that hide online status.
func (m *Manager) Activity(remoteUserID string, at time.Time) {
	if m == nil {
		return
	}
	now := m.now()
	if at.IsZero() || at.After(now) {
		at = now
	}
	until := at.Add(ActivityOnline)
	if !until.After(now) {
		return // old news (backfill): nothing to say about now
	}
	m.lock.Lock()
	e, ok := m.entries[remoteUserID]
	// Don't cut short a longer online the network itself reported.
	longer := ok && e.desired == event.PresenceOnline && (e.until.IsZero() || e.until.After(until))
	m.lock.Unlock()
	if !longer {
		m.Update(remoteUserID, State{Presence: event.PresenceOnline, Until: until})
	}
}

// evictOne drops a settled, non-online entry. Must hold the lock.
func (m *Manager) evictOne() bool {
	var oldestKey string
	var oldest time.Time
	for k, e := range m.entries {
		if e.desired != event.PresenceOnline && e.sent == e.desired &&
			(oldestKey == "" || e.touched.Before(oldest)) {
			oldestKey, oldest = k, e.touched
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(m.entries, oldestKey)
	return true
}

type pending struct {
	key      string
	presence event.Presence
	sentAt   time.Time
}

// Tick runs one iteration of the worker loop. Exported for tests.
func (m *Manager) Tick(ctx context.Context) {
	now := m.now()
	var due []pending
	m.lock.Lock()
	for k, e := range m.entries {
		if e.desired == event.PresenceOnline && !e.until.IsZero() && !now.Before(e.until) {
			e.desired = event.PresenceOffline
			e.until = time.Time{}
		}
		sinceSent := now.Sub(e.sentAt)
		switch {
		case e.desired != e.sent && sinceSent >= m.cfg.Debounce:
		case e.desired == event.PresenceOnline && e.sent == event.PresenceOnline && sinceSent >= m.cfg.Refresh:
		default:
			if e.desired != event.PresenceOnline && e.sent == e.desired && now.Sub(e.touched) > time.Hour {
				delete(m.entries, k)
			}
			continue
		}
		due = append(due, pending{key: k, presence: e.desired, sentAt: e.sentAt})
	}
	m.lock.Unlock()
	if len(due) == 0 {
		return
	}
	// Transitions to online first, then whatever waited longest.
	sort.Slice(due, func(i, j int) bool {
		oi, oj := due[i].presence == event.PresenceOnline, due[j].presence == event.PresenceOnline
		if oi != oj {
			return oi
		}
		return due[i].sentAt.Before(due[j].sentAt)
	})
	log := zerolog.Ctx(ctx)
	for i, p := range due {
		if !m.limiter.Allow(m.now()) {
			log.Debug().Int("deferred", len(due)-i).Msg("Presence rate limit reached, deferring")
			return
		}
		err := m.send(ctx, p.key, p.presence)
		m.lock.Lock()
		if e, ok := m.entries[p.key]; ok {
			// On error, still bump sentAt so the debounce acts as a backoff.
			e.sentAt = m.now()
			if err == nil {
				e.sent = p.presence
			}
		}
		m.lock.Unlock()
		if err != nil {
			log.Warn().Err(err).Str("remote_user_id", p.key).Str("presence", string(p.presence)).
				Msg("Failed to bridge presence")
		} else {
			log.Debug().Str("remote_user_id", p.key).Str("presence", string(p.presence)).
				Msg("Bridged presence")
		}
	}
}

// Run drives the worker loop until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(ctx)
		}
	}
}

// TokenBucket is a minimal clock-injected rate limiter.
type TokenBucket struct {
	lock   sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func NewTokenBucket(ratePerSecond float64, burst int, now time.Time) *TokenBucket {
	return &TokenBucket{rate: ratePerSecond, burst: float64(burst), tokens: float64(burst), last: now}
}

func (tb *TokenBucket) Allow(now time.Time) bool {
	tb.lock.Lock()
	defer tb.lock.Unlock()
	if elapsed := now.Sub(tb.last).Seconds(); elapsed > 0 {
		tb.tokens = min(tb.burst, tb.tokens+elapsed*tb.rate)
		tb.last = now
	}
	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}
