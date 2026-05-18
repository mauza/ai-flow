package workqueue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Work is one unit of queued orchestration work.
type Work func(context.Context)

// Queue runs orchestration work with bounded concurrency.
type Queue struct {
	jobs    chan Work
	workers int
	wg      sync.WaitGroup
}

// New creates a bounded work queue.
func New(workers, capacity int) *Queue {
	if workers < 1 {
		workers = 1
	}
	if capacity < workers {
		capacity = workers
	}
	return &Queue{
		jobs:    make(chan Work, capacity),
		workers: workers,
	}
}

// Start launches workers. Workers stop when ctx is cancelled.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.runWorker(ctx, i+1)
	}
}

func (q *Queue) runWorker(ctx context.Context, id int) {
	defer q.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case work := <-q.jobs:
			if work == nil {
				continue
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("work item panicked", "worker", id, "panic", r)
					}
				}()
				work(ctx)
			}()
		}
	}
}

// Enqueue adds work to the queue, applying backpressure when the queue is full.
func (q *Queue) Enqueue(ctx context.Context, work Work) error {
	if work == nil {
		return fmt.Errorf("work item is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case q.jobs <- work:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait blocks until all workers have stopped.
func (q *Queue) Wait() {
	q.wg.Wait()
}
