package relay

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrQueueFull = errors.New("relay queue is full")

type ticket struct{ identity byte }

// A permit covers all upstream attempts and the complete streaming body.
type scheduler struct {
	mu         sync.Mutex
	changed    chan struct{}
	waiters    []*ticket
	active     int
	concurrent int
	queueLimit int
	interval   time.Duration
	nextStart  time.Time
	cooldown   time.Time
}

func newScheduler(concurrent, queue int, interval time.Duration) *scheduler {
	return &scheduler{changed: make(chan struct{}), concurrent: concurrent, queueLimit: queue, interval: interval}
}

func (s *scheduler) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *scheduler) acquire(ctx context.Context) (func(), error) {
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.active < s.concurrent && len(s.waiters) == 0 {
		s.active++
		s.mu.Unlock()
		return s.releaseOnce(), nil
	}
	if len(s.waiters) >= s.queueLimit {
		s.mu.Unlock()
		return nil, ErrQueueFull
	}
	entry := &ticket{}
	s.waiters = append(s.waiters, entry)
	for {
		if err := ctx.Err(); err != nil {
			for i, candidate := range s.waiters {
				if candidate == entry {
					s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
					break
				}
			}
			s.signal()
			s.mu.Unlock()
			return nil, err
		}
		if s.waiters[0] == entry && s.active < s.concurrent {
			s.waiters = s.waiters[1:]
			s.active++
			s.signal()
			s.mu.Unlock()
			return s.releaseOnce(), nil
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		s.mu.Lock()
	}
}

func (s *scheduler) releaseOnce() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.active--
			s.signal()
			s.mu.Unlock()
		})
	}
}

// Every upstream attempt, including retries, shares the same pacing and cooldown.
func (s *scheduler) waitAttempt(ctx context.Context) error {
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		ready := s.nextStart
		if s.cooldown.After(ready) {
			ready = s.cooldown
		}
		delay := time.Until(ready)
		if delay <= 0 {
			s.nextStart = time.Now().Add(s.interval)
			s.signal()
			s.mu.Unlock()
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (s *scheduler) coolUntil(until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.After(s.cooldown) {
		s.cooldown = until
		s.signal()
	}
}

type ScheduleStatus struct {
	Active          int        `json:"active"`
	Queued          int        `json:"queued"`
	MaxConcurrent   int        `json:"max_concurrent"`
	MaxQueue        int        `json:"max_queue"`
	CooldownUntil   *time.Time `json:"cooldown_until,omitempty"`
	CooldownSeconds float64    `json:"cooldown_seconds"`
}

func (s *scheduler) status() ScheduleStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := ScheduleStatus{Active: s.active, Queued: len(s.waiters), MaxConcurrent: s.concurrent, MaxQueue: s.queueLimit}
	if remaining := time.Until(s.cooldown); remaining > 0 {
		until := s.cooldown.UTC()
		status.CooldownUntil = &until
		status.CooldownSeconds = remaining.Seconds()
	}
	return status
}
