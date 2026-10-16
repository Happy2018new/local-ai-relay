package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestSchedulerFIFOQueueLimitAndCancellation(t *testing.T) {
	s := newScheduler(1, 2, 0)
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		r, err := s.acquire(firstCtx)
		if r != nil {
			r()
		}
		firstResult <- err
	}()
	eventually(t, func() bool { return s.status().Queued == 1 })
	second := make(chan func(), 1)
	go func() {
		r, _ := s.acquire(context.Background())
		second <- r
	}()
	eventually(t, func() bool { return s.status().Queued == 2 })
	if _, err := s.acquire(context.Background()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected queue full, got %v", err)
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled, got %v", err)
	}
	release()
	release()
	select {
	case done := <-second:
		if done == nil {
			t.Fatal("queued request not admitted")
		}
		done()
	case <-time.After(3 * time.Second):
		t.Fatal("canceled waiter blocked next request")
	}
	if status := s.status(); status.Active != 0 || status.Queued != 0 {
		t.Fatalf("leaked scheduler state: %+v", status)
	}
}

func TestSchedulerPacingAndCooldownExtension(t *testing.T) {
	s := newScheduler(2, 1, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.waitAttempt(ctx); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := s.waitAttempt(ctx); err != nil || time.Since(started) < 40*time.Millisecond {
		t.Fatalf("pacing not enforced: %v", err)
	}
	s.coolUntil(time.Now().Add(70 * time.Millisecond))
	done := make(chan error, 1)
	go func() { done <- s.waitAttempt(ctx) }()
	time.Sleep(20 * time.Millisecond)
	extended := time.Now().Add(100 * time.Millisecond)
	s.coolUntil(extended)
	if err := <-done; err != nil || time.Now().Before(extended) {
		t.Fatalf("cooldown extension not respected: %v", err)
	}
}
