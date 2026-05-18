package workqueue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueRunsWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := New(1, 1)
	q.Start(ctx)

	done := make(chan struct{})
	if err := q.Enqueue(ctx, func(context.Context) {
		close(done)
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued work did not run")
	}

	cancel()
	q.Wait()
}

func TestQueueLimitsConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q := New(2, 4)
	q.Start(ctx)

	start := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var running int32
	var maxRunning int32

	for i := 0; i < 4; i++ {
		if err := q.Enqueue(ctx, func(context.Context) {
			<-start
			current := atomic.AddInt32(&running, 1)
			for {
				max := atomic.LoadInt32(&maxRunning)
				if current <= max || atomic.CompareAndSwapInt32(&maxRunning, max, current) {
					break
				}
			}
			<-release
			atomic.AddInt32(&running, -1)
			done <- struct{}{}
		}); err != nil {
			t.Fatalf("Enqueue returned error: %v", err)
		}
	}

	close(start)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&maxRunning); got != 2 {
		t.Fatalf("max concurrent work = %d, want 2", got)
	}

	close(release)
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("queued work did not finish")
		}
	}

	cancel()
	q.Wait()
}

func TestEnqueueReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	q := New(1, 1)
	if err := q.Enqueue(ctx, func(context.Context) {}); err == nil {
		t.Fatal("Enqueue returned nil error, want context error")
	}
}
