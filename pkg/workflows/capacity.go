package workflows

import (
	"context"
	"fmt"
	"sync"
)

// Capacity shares one execution boundary across workflows, workers and broker
// callbacks. Slots remains exposed for compatibility with existing callers.
type Capacity struct {
	Slots        chan struct{}
	MaxSuspended int
	mu           sync.Mutex
	suspended    int
}

type CapacityLease struct {
	pool    *Capacity
	mu      sync.Mutex
	state   string // held, suspended, released
	done    chan struct{}
	changed chan struct{}
}

func (p *Capacity) Acquire(ctx context.Context) (*CapacityLease, error) {
	if p.Slots == nil || cap(p.Slots) == 0 {
		return nil, fmt.Errorf("execution capacity must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case p.Slots <- struct{}{}:
		return p.lease(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Capacity) TryAcquire() (*CapacityLease, bool) {
	select {
	case p.Slots <- struct{}{}:
		return p.lease(), true
	default:
		return nil, false
	}
}

func (p *Capacity) lease() *CapacityLease {
	return &CapacityLease{pool: p, state: "held", done: make(chan struct{}), changed: make(chan struct{})}
}
func (l *CapacityLease) notifyLocked() { close(l.changed); l.changed = make(chan struct{}) }

// Suspend is called only after an adapter's explicit invocation-level blocked
// barrier, never just because one callback is waiting. It is idempotent across
// parallel callbacks and bounded independently of runnable execution slots.
func (l *CapacityLease) Suspend() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == "released" {
		return fmt.Errorf("execution lease is released")
	}
	if l.state == "suspended" {
		return nil
	}
	if l.state == "acquiring" {
		return fmt.Errorf("capacity reacquisition is already in progress")
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	limit := l.pool.MaxSuspended
	if limit == 0 {
		limit = 128
	}
	if l.pool.suspended >= limit {
		return fmt.Errorf("suspended invocation limit reached")
	}
	l.pool.suspended++
	<-l.pool.Slots
	l.state = "suspended"
	return nil
}

func (l *CapacityLease) Reacquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		switch l.state {
		case "released":
			l.mu.Unlock()
			return fmt.Errorf("execution lease is released")
		case "held":
			l.mu.Unlock()
			return nil
		case "acquiring":
			changed := l.changed
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.done:
				return fmt.Errorf("execution lease is released")
			case <-changed:
				continue
			}
		}
		l.state = "acquiring"
		l.mu.Unlock()
		acquired := false
		select {
		case l.pool.Slots <- struct{}{}:
			acquired = true
		case <-ctx.Done():
		case <-l.done:
		}
		l.mu.Lock()
		if l.state == "released" {
			if acquired {
				<-l.pool.Slots
			}
			l.mu.Unlock()
			return fmt.Errorf("execution lease is released")
		}
		if acquired {
			l.pool.mu.Lock()
			l.pool.suspended--
			l.pool.mu.Unlock()
			l.state = "held"
		} else {
			l.state = "suspended"
		}
		l.notifyLocked()
		l.mu.Unlock()
		if !acquired {
			return ctx.Err()
		}
		return nil
	}
}

func (l *CapacityLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch l.state {
	case "held":
		<-l.pool.Slots
	case "suspended", "acquiring":
		l.pool.mu.Lock()
		l.pool.suspended--
		l.pool.mu.Unlock()
	}
	if l.state != "released" {
		l.state = "released"
		close(l.done)
		l.notifyLocked()
	}
}

func (p *Capacity) Suspended() int { p.mu.Lock(); defer p.mu.Unlock(); return p.suspended }
