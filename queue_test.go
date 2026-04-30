package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestQM(t *testing.T) *QueueManager {
	t.Helper()
	qm := NewQueueManager([]string{"queued (position {position})"}, "started", 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 1}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })
	return qm
}

func waitForNotRunning(t *testing.T, qm *QueueManager, net, ch, user string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if !qm.IsRunning(net, ch, user) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for job to complete")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestQueueManager_BasicEnqueueAndRun(t *testing.T) {
	qm := newTestQM(t)

	var ran atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		ran.Store(true)
		wg.Done()
	})

	if pos != 0 {
		t.Errorf("first enqueue position = %d, want 0", pos)
	}

	wg.Wait()

	if !ran.Load() {
		t.Error("job did not run")
	}

	waitForNotRunning(t, qm, "net", "#chan", "user")
	if qm.IsRunning("net", "#chan", "user") {
		t.Error("should not be running after job completes")
	}
}

func TestQueueManager_EnqueueWhileBusy(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg1, wg2 sync.WaitGroup
	wg1.Add(1)
	wg2.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		wg1.Done()
		<-unblock
	})

	wg1.Wait()

	pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		wg2.Done()
	})

	if pos != 1 {
		t.Errorf("second enqueue position = %d, want 1", pos)
	}

	if !qm.IsRunning("net", "#chan", "user") {
		t.Error("first job should be running")
	}

	current, pending := qm.QueueStatus("net", "#chan", "user")
	if current == nil {
		t.Fatal("expected current item")
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	close(unblock)
	wg2.Wait()

	waitForNotRunning(t, qm, "net", "#chan", "user")
	if qm.IsRunning("net", "#chan", "user") {
		t.Error("should not be running after both jobs complete")
	}
}

func TestQueueManager_MaxDepth(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Done()
		<-unblock
	})
	started.Wait()

	for i := 0; i < 4; i++ {
		pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})
		if pos != i+1 {
			t.Errorf("enqueue %d position = %d, want %d", i+1, pos, i+1)
		}
	}

	pos := qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})
	if pos != -1 {
		t.Errorf("overflow enqueue position = %d, want -1", pos)
	}

	close(unblock)
}

func TestQueueManager_StopCurrent(t *testing.T) {
	qm := newTestQM(t)

	var started atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Store(true)
		<-ctx.Done()
		wg.Done()
	})

	for !started.Load() {
		time.Sleep(10 * time.Millisecond)
	}

	if !qm.StopCurrent("net", "#chan") {
		t.Error("StopCurrent should return true for running job")
	}

	wg.Wait()

	waitForNotRunning(t, qm, "net", "#chan", "user")
	if qm.StopCurrent("net", "#chan") {
		t.Error("StopCurrent should return false when no job running")
	}
}

func TestQueueManager_StopCurrentStartsNext(t *testing.T) {
	qm := newTestQM(t)

	unblockFirst := make(chan struct{})
	var firstDone, secondRan atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		select {
		case <-unblockFirst:
		case <-ctx.Done():
		}
		firstDone.Store(true)
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		secondRan.Store(true)
		wg.Done()
	})

	qm.StopCurrent("net", "#chan")

	wg.Wait()

	if !firstDone.Load() {
		t.Error("first job should have completed (via cancel)")
	}
	if !secondRan.Load() {
		t.Error("second job should have run after first was cancelled")
	}
}

func TestQueueManager_DifferentChannelsParallel(t *testing.T) {
	qm := NewQueueManager([]string{"queued"}, "started", 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	var ch1Ran, ch2Ran atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	unblock := make(chan struct{})

	qm.Enqueue("net", "#chan1", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		ch1Ran.Store(true)
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan2", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		ch2Ran.Store(true)
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	if !ch1Ran.Load() {
		t.Error("chan1 job should have started")
	}
	if !ch2Ran.Load() {
		t.Error("chan2 job should start in parallel (different channel)")
	}

	close(unblock)
	wg.Wait()
}

func TestQueueManager_StopCancelsRunning(t *testing.T) {
	qm := newTestQM(t)

	var cancelled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-ctx.Done()
		cancelled.Store(true)
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)
	qm.Stop()
	wg.Wait()

	if !cancelled.Load() {
		t.Error("job should have been cancelled by Stop()")
	}
}

func TestQueueManager_IsRunning(t *testing.T) {
	qm := newTestQM(t)

	if qm.IsRunning("net", "#chan", "user") {
		t.Error("should not be running initially")
	}

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	if !qm.IsRunning("net", "#chan", "user") {
		t.Error("should be running during execution")
	}

	close(unblock)
	wg.Wait()

	waitForNotRunning(t, qm, "net", "#chan", "user")
	if qm.IsRunning("net", "#chan", "user") {
		t.Error("should not be running after completion")
	}
}

func TestQueueManager_UpdateServiceLimits(t *testing.T) {
	qm := newTestQM(t)

	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})

	var user1Started, user2Started atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)
	unblock := make(chan struct{})

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		user1Started.Store(true)
		<-unblock
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		user2Started.Store(true)
		<-unblock
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	if !user1Started.Load() {
		t.Error("user1 should have started")
	}
	if !user2Started.Load() {
		t.Error("user2 should start with parallel=2 (same channel)")
	}

	close(unblock)
	wg.Wait()
}

func TestQueueManager_QueueStatus(t *testing.T) {
	qm := newTestQM(t)

	current, pending := qm.QueueStatus("net", "#chan", "user")
	if current != nil || pending != nil {
		t.Error("expected nil status for unknown user")
	}

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	current, pending = qm.QueueStatus("net", "#chan", "user")
	if current == nil {
		t.Fatal("expected current item")
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending, got %d", len(pending))
	}

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})
	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})

	current, pending = qm.QueueStatus("net", "#chan", "user")
	if current == nil {
		t.Fatal("expected current item")
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}

	close(unblock)
	wg.Wait()
}

func TestQueueManager_FIFOOrder(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)

	var order []int
	var orderMu sync.Mutex

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		orderMu.Lock()
		order = append(order, 1)
		orderMu.Unlock()
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, 2)
		orderMu.Unlock()
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, 3)
		orderMu.Unlock()
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, 4)
		orderMu.Unlock()
		wg.Done()
	})

	close(unblock)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 4 {
		t.Fatalf("expected 4 executions, got %d", len(order))
	}
	for i, want := range []int{1, 2, 3, 4} {
		if order[i] != want {
			t.Errorf("execution[%d] = %d, want %d", i, order[i], want)
		}
	}
}

func TestQueueManager_CrossUserFIFOOrder(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	var order []string
	var orderMu sync.Mutex

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		orderMu.Lock()
		order = append(order, "user1")
		orderMu.Unlock()
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, "user2")
		orderMu.Unlock()
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user3", "svc", "", func(ctx context.Context, output chan<- string) {
		orderMu.Lock()
		order = append(order, "user3")
		orderMu.Unlock()
		wg.Done()
	})

	close(unblock)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 executions, got %d: %v", len(order), order)
	}
	want := []string{"user1", "user2", "user3"}
	for i, w := range want {
		if order[i] != w {
			t.Errorf("execution[%d] = %q, want %q", i, order[i], w)
		}
	}
}

func TestQueueManager_ConcurrentExecutionFIFODelivery(t *testing.T) {
	qm := NewQueueManager([]string{"queued"}, "started", 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 2}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	var execOrder []string
	var execMu sync.Mutex

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		execMu.Lock()
		execOrder = append(execOrder, "user1")
		execMu.Unlock()
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		execMu.Lock()
		execOrder = append(execOrder, "user2")
		execMu.Unlock()
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	execMu.Lock()
	if len(execOrder) != 2 {
		t.Fatalf("expected both jobs to start concurrently, got %d: %v", len(execOrder), execOrder)
	}
	execMu.Unlock()

	close(unblock)
	wg.Wait()
}

func TestQueueManager_ServiceParallel1(t *testing.T) {
	qm := newTestQM(t)

	var user1Started, user2Started atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	unblock := make(chan struct{})

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		user1Started.Store(true)
		<-unblock
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		user2Started.Store(true)
		wg.Done()
	})

	time.Sleep(100 * time.Millisecond)

	if !user1Started.Load() {
		t.Error("user1 should have started")
	}
	if user2Started.Load() {
		t.Error("user2 should NOT start while user1 holds the service slot (parallel=1)")
	}

	close(unblock)
	wg.Wait()

	if !user2Started.Load() {
		t.Error("user2 should have started after user1 completed")
	}
}

func TestQueueManager_ServiceParallel0(t *testing.T) {
	qm := NewQueueManager([]string{"queued"}, "started", 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 0}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	var count atomic.Int32
	var wg sync.WaitGroup
	wg.Add(5)

	unblock := make(chan struct{})

	for i := 0; i < 5; i++ {
		qm.Enqueue("net", "#chan", fmt.Sprintf("user%d", i), "svc", "", func(ctx context.Context, output chan<- string) {
			count.Add(1)
			<-unblock
			wg.Done()
		})
	}

	time.Sleep(100 * time.Millisecond)

	started := count.Load()
	if started != 5 {
		t.Errorf("expected all 5 jobs to start with parallel=0 (unlimited), got %d", started)
	}

	close(unblock)
	wg.Wait()
}

func TestQueueManager_CancellationPropagation(t *testing.T) {
	qm := newTestQM(t)

	var gotCancelled atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		select {
		case <-time.After(5 * time.Second):
			t.Error("should have been cancelled")
		case <-ctx.Done():
			gotCancelled.Store(true)
		}
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)
	qm.StopCurrent("net", "#chan")
	wg.Wait()

	if !gotCancelled.Load() {
		t.Error("Execute should have received ctx.Done() after StopCurrent")
	}
}

func TestQueueManager_SchedulerFairness(t *testing.T) {
	qm := NewQueueManager([]string{"queued"}, "started", 5)
	qm.UpdateServiceLimits(map[string]Service{"svc": {Parallel: 1}})
	qm.Start()
	t.Cleanup(func() { qm.Stop() })

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	var order []string
	var orderMu sync.Mutex
	addOrder := func(name string) {
		orderMu.Lock()
		order = append(order, name)
		orderMu.Unlock()
	}

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		addOrder("user1-first")
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		addOrder("user1-second")
		wg.Done()
	})
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		addOrder("user2-first")
		wg.Done()
	})

	close(unblock)
	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 executions, got %d: %v", len(order), order)
	}
	if order[0] != "user1-first" {
		t.Errorf("first = %q, want user1-first", order[0])
	}
}

func TestQueueManager_StopEmptyQueue(t *testing.T) {
	qm := newTestQM(t)

	if qm.StopCurrent("net", "#chan") {
		t.Error("StopCurrent on empty queue should return false")
	}
}

func TestQueueManager_CancelPending(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Done()
		<-unblock
	})
	started.Wait()

	qm.Enqueue("net", "#chan", "user", "svc", "", func(ctx context.Context, output chan<- string) {})

	_, pending := qm.QueueStatus("net", "#chan", "user")
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}

	if !qm.CancelPending("net", "#chan", "user") {
		t.Error("CancelPending should return true when items removed")
	}

	_, pending = qm.QueueStatus("net", "#chan", "user")
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after cancel, got %d", len(pending))
	}

	if qm.CancelPending("net", "#chan", "nobody") {
		t.Error("CancelPending should return false when no items match")
	}

	close(unblock)
}

func TestQueueManager_CancelPendingOtherUserPreserved(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		started.Done()
		<-unblock
	})
	started.Wait()

	var user2Ran atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {
		user2Ran.Store(true)
		wg.Done()
	})

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {})

	qm.CancelPending("net", "#chan", "user1")

	close(unblock)
	wg.Wait()

	if !user2Ran.Load() {
		t.Error("user2's pending job should still run after cancelling user1's pending items")
	}
}

func TestQueueManager_QueueStatusOtherUser(t *testing.T) {
	qm := newTestQM(t)

	unblock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	qm.Enqueue("net", "#chan", "user1", "svc", "", func(ctx context.Context, output chan<- string) {
		<-unblock
		wg.Done()
	})

	time.Sleep(50 * time.Millisecond)

	qm.Enqueue("net", "#chan", "user2", "svc", "", func(ctx context.Context, output chan<- string) {})

	current1, pending1 := qm.QueueStatus("net", "#chan", "user1")
	if current1 == nil {
		t.Fatal("user1 should see current item (their running job)")
	}
	if len(pending1) != 0 {
		t.Errorf("user1 should see 0 pending, got %d", len(pending1))
	}

	current2, pending2 := qm.QueueStatus("net", "#chan", "user2")
	if current2 != nil {
		t.Error("user2 should not see current item (user1 is running)")
	}
	if len(pending2) != 1 {
		t.Errorf("user2 should see 1 pending, got %d", len(pending2))
	}

	close(unblock)
	wg.Wait()
}
