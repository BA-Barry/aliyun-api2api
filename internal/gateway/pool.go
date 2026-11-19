package gateway

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"aliyun-api2api/internal/config"
)

var (
	ErrNoUpstream       = errors.New("no healthy upstream is currently available")
	ErrAllUpstreamsBusy = errors.New("all healthy upstreams reached their concurrency limit")
)

type FailureKind int

const (
	FailureTransient FailureKind = iota
	FailureCredential
)

type Account struct {
	ID               string
	APIKey           string
	OpenAIBaseURL    string
	AnthropicBaseURL string
	Weight           int
	Enabled          bool

	mu                sync.Mutex
	inflight          int
	consecutiveFails  int
	cooldownUntil     time.Time
	lastFailureReason string
	requests          uint64
	failures          uint64
}

type Pool struct {
	accounts           []*Account
	ring               []*Account
	strategy           string
	baseCooldown       time.Duration
	credentialCooldown time.Duration
	maxInflight        int
	cursor             atomic.Uint64
}

type Lease struct {
	Account *Account
	once    sync.Once
}

type AccountStatus struct {
	ID                string    `json:"id"`
	Enabled           bool      `json:"enabled"`
	Weight            int       `json:"weight"`
	Inflight          int       `json:"inflight"`
	ConsecutiveFails  int       `json:"consecutive_failures"`
	CooldownUntil     time.Time `json:"cooldown_until,omitempty"`
	LastFailureReason string    `json:"last_failure_reason,omitempty"`
	Requests          uint64    `json:"requests"`
	Failures          uint64    `json:"failures"`
}

func NewPool(cfg config.Config) *Pool {
	p := &Pool{
		strategy:           cfg.Strategy,
		baseCooldown:       cfg.BaseCooldown.Duration,
		credentialCooldown: cfg.CredentialCooldown.Duration,
		maxInflight:        cfg.MaxConcurrentPerUpstream,
	}
	for _, item := range cfg.Upstreams {
		enabled := item.Enabled == nil || *item.Enabled
		a := &Account{
			ID:               item.ID,
			APIKey:           item.APIKey,
			OpenAIBaseURL:    item.OpenAIBaseURL,
			AnthropicBaseURL: item.AnthropicBaseURL,
			Weight:           item.Weight,
			Enabled:          enabled,
		}
		p.accounts = append(p.accounts, a)
		if enabled {
			for i := 0; i < a.Weight; i++ {
				p.ring = append(p.ring, a)
			}
		}
	}
	return p
}

func (p *Pool) Acquire(excluded map[string]struct{}) (*Lease, error) {
	now := time.Now()
	if p.strategy == "round_robin" {
		return p.acquireRoundRobin(now, excluded)
	}
	return p.acquireLeastInflight(now, excluded)
}

func (p *Pool) acquireRoundRobin(now time.Time, excluded map[string]struct{}) (*Lease, error) {
	if len(p.ring) == 0 {
		return nil, ErrNoUpstream
	}
	start := int(p.cursor.Add(1)-1) % len(p.ring)
	hasHealthy := false
	for i := 0; i < len(p.ring); i++ {
		a := p.ring[(start+i)%len(p.ring)]
		if _, skip := excluded[a.ID]; skip {
			continue
		}
		a.mu.Lock()
		if a.Enabled && !now.Before(a.cooldownUntil) {
			hasHealthy = true
			if a.inflight >= p.maxInflight {
				a.mu.Unlock()
				continue
			}
			a.inflight++
			a.requests++
			a.mu.Unlock()
			return &Lease{Account: a}, nil
		}
		a.mu.Unlock()
	}
	if hasHealthy {
		return nil, ErrAllUpstreamsBusy
	}
	return nil, ErrNoUpstream
}

func (p *Pool) acquireLeastInflight(now time.Time, excluded map[string]struct{}) (*Lease, error) {
	if len(p.accounts) == 0 {
		return nil, ErrNoUpstream
	}
	for {
		start := int(p.cursor.Add(1)-1) % len(p.accounts)
		var best *Account
		bestInflight, bestWeight := 0, 1
		hasHealthy := false
		for i := 0; i < len(p.accounts); i++ {
			a := p.accounts[(start+i)%len(p.accounts)]
			if _, skip := excluded[a.ID]; skip {
				continue
			}
			a.mu.Lock()
			healthy := a.Enabled && !now.Before(a.cooldownUntil)
			inflight, weight := a.inflight, a.Weight
			a.mu.Unlock()
			if !healthy {
				continue
			}
			hasHealthy = true
			if inflight >= p.maxInflight {
				continue
			}
			// Compare inflight/weight without floating point. Rotation makes ties fair.
			if best == nil || inflight*bestWeight < bestInflight*weight {
				best, bestInflight, bestWeight = a, inflight, weight
			}
		}
		if best == nil {
			if hasHealthy {
				return nil, ErrAllUpstreamsBusy
			}
			return nil, ErrNoUpstream
		}
		best.mu.Lock()
		if best.Enabled && !now.Before(best.cooldownUntil) && best.inflight < p.maxInflight {
			best.inflight++
			best.requests++
			best.mu.Unlock()
			return &Lease{Account: best}, nil
		}
		best.mu.Unlock()
		// Its state changed between selection and acquisition. Retry without
		// recursion so contention cannot grow the call stack.
		now = time.Now()
	}
}

func (l *Lease) Release() {
	if l == nil || l.Account == nil {
		return
	}
	l.once.Do(func() {
		l.Account.mu.Lock()
		if l.Account.inflight > 0 {
			l.Account.inflight--
		}
		l.Account.mu.Unlock()
	})
}

func (p *Pool) MarkSuccess(a *Account) {
	a.mu.Lock()
	a.consecutiveFails = 0
	a.cooldownUntil = time.Time{}
	a.lastFailureReason = ""
	a.mu.Unlock()
}

func (p *Pool) MarkFailure(a *Account, kind FailureKind, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures++
	a.consecutiveFails++
	a.lastFailureReason = reason
	cooldown := p.baseCooldown
	if kind == FailureCredential {
		cooldown = p.credentialCooldown
	} else {
		shift := a.consecutiveFails - 1
		if shift > 5 {
			shift = 5
		}
		cooldown *= time.Duration(1 << shift)
		if cooldown > p.credentialCooldown {
			cooldown = p.credentialCooldown
		}
	}
	a.cooldownUntil = time.Now().Add(cooldown)
}

func (p *Pool) Status() []AccountStatus {
	out := make([]AccountStatus, 0, len(p.accounts))
	for _, a := range p.accounts {
		a.mu.Lock()
		out = append(out, AccountStatus{
			ID:                a.ID,
			Enabled:           a.Enabled,
			Weight:            a.Weight,
			Inflight:          a.inflight,
			ConsecutiveFails:  a.consecutiveFails,
			CooldownUntil:     a.cooldownUntil,
			LastFailureReason: a.lastFailureReason,
			Requests:          a.requests,
			Failures:          a.failures,
		})
		a.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
