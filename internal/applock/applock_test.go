package applock

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestTryLock(t *testing.T) {
	var locks Locks

	unlock, ok := locks.TryLock("web")
	if !ok {
		t.Fatal("TryLock() on a free app failed")
	}
	if _, ok := locks.TryLock("web"); ok {
		t.Error("TryLock() on a busy app succeeded")
	}
	// Another app is independent.
	unlockOther, ok := locks.TryLock("api")
	if !ok {
		t.Error("TryLock() on another app failed")
	}
	defer unlockOther()

	unlock()
	second, ok := locks.TryLock("web")
	if !ok {
		t.Fatal("TryLock() after unlock failed")
	}

	// Calling the first unlock again must not free the app that is now
	// held by the second lock.
	unlock()
	if _, ok := locks.TryLock("web"); ok {
		t.Error("a repeated unlock released a lock held by somebody else")
	}
	second()
}

func TestOnlyOneOfManyWins(t *testing.T) {
	var locks Locks
	var winners atomic.Int32
	var wg sync.WaitGroup

	// A gate that releases all goroutines at the same moment, to make them
	// collide as hard as possible.
	start := make(chan struct{})
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := locks.TryLock("web"); ok {
				winners.Add(1)
			}
		}()
	}
	// Closing a channel wakes up every goroutine waiting on it.
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Errorf("%d goroutines acquired the lock, want exactly 1", got)
	}
}
