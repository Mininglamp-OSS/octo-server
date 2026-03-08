package pool

import (
	"sync"
	"testing"
	"time"
)

func TestLoopPopNilSafety(t *testing.T) {
	// Test that loopPop exits gracefully when queue is closed and returns nil
	q := NewQueue()

	// Push a job
	job := &Job{
		Data: "test",
		JobFunc: func(id int64, data interface{}) {
			// no-op
		},
	}
	q.Push(job)

	// Pop should return the job
	popped := q.Pop()
	if popped == nil {
		t.Fatal("expected job, got nil")
	}

	poppedJob, ok := popped.(*Job)
	if !ok {
		t.Fatal("expected *Job type")
	}
	if poppedJob.Data != "test" {
		t.Fatalf("expected job Data 'test', got %v", poppedJob.Data)
	}

	// Close queue
	q.Close()

	// Pop should return nil when queue is closed
	nilResult := q.Pop()
	if nilResult != nil {
		t.Fatalf("expected nil after close, got %v", nilResult)
	}
}

func TestLoopPopExitsOnQueueClose(t *testing.T) {
	// Create a minimal test to verify loopPop exits when queue is closed
	q := NewQueue()

	var wg sync.WaitGroup
	loopExited := make(chan struct{})

	// Simulate loopPop behavior
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(loopExited)
		for {
			jobObj := q.Pop()
			if jobObj == nil {
				// Queue is closed, exit loop
				return
			}
			_, ok := jobObj.(*Job)
			if !ok {
				continue
			}
		}
	}()

	// Close the queue
	q.Close()

	// Wait for loop to exit with timeout
	select {
	case <-loopExited:
		// Success - loop exited gracefully
	case <-time.After(2 * time.Second):
		t.Fatal("loopPop did not exit after queue close")
	}

	wg.Wait()
}

func TestCollectorQueueCloseGraceful(t *testing.T) {
	// Test that a collector's queue closure doesn't cause panic
	q := NewQueue()

	// Push some jobs
	for i := 0; i < 3; i++ {
		q.Push(&Job{
			Data: i,
			JobFunc: func(id int64, data interface{}) {
				// no-op
			},
		})
	}

	// Pop all jobs
	for i := 0; i < 3; i++ {
		job := q.Pop()
		if job == nil {
			t.Fatalf("expected job at index %d, got nil", i)
		}
	}

	// Close queue
	q.Close()

	// Multiple pops after close should all return nil without panic
	for i := 0; i < 5; i++ {
		result := q.Pop()
		if result != nil {
			t.Fatalf("expected nil after close (attempt %d), got %v", i, result)
		}
	}
}

func TestTypeAssertionSafety(t *testing.T) {
	// Test that invalid types are handled safely
	q := NewQueue()

	// Push non-Job type
	q.Push("not a job")
	q.Push(12345)
	q.Push(&Job{Data: "valid"})

	// Pop should return items, type assertion should be checked
	item1 := q.Pop()
	if item1 == nil {
		t.Fatal("expected item, got nil")
	}
	if _, ok := item1.(*Job); ok {
		t.Fatal("string should not be *Job")
	}

	item2 := q.Pop()
	if item2 == nil {
		t.Fatal("expected item, got nil")
	}
	if _, ok := item2.(*Job); ok {
		t.Fatal("int should not be *Job")
	}

	item3 := q.Pop()
	if item3 == nil {
		t.Fatal("expected item, got nil")
	}
	job, ok := item3.(*Job)
	if !ok {
		t.Fatal("expected *Job type for valid job")
	}
	if job.Data != "valid" {
		t.Fatalf("expected job Data 'valid', got %v", job.Data)
	}
}
