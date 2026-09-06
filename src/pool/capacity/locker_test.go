package capacity

import (
	"sync"
	"testing"
)

func TestLockerSerializesSamePoolAndReleasesEntries(t *testing.T) {
	locker := &Locker{}
	unlock := locker.Lock("pool-a")
	acquired := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		release := locker.Lock("pool-a")
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("same Pool lock was acquired concurrently")
	default:
	}
	unlock()
	wait.Wait()
	if len(locker.entries) != 0 {
		t.Fatalf("lock entries remain: %d", len(locker.entries))
	}
}

func TestLockerAllowsIndependentPools(t *testing.T) {
	locker := &Locker{}
	unlockA := locker.Lock("pool-a")
	unlockB := locker.Lock("pool-b")
	unlockB()
	unlockA()
}
