package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/watch"
)

func TestReconcileWakeCoalescesConcurrentEvents(t *testing.T) {
	wake := make(chan struct{}, 1)
	for range 100 {
		notifyReconciler(wake)
	}
	if len(wake) != 1 {
		t.Fatalf("coalesced wake count = %d, want 1", len(wake))
	}
}

func TestEventWatchReconnectsAfterClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	first := watch.NewRaceFreeFake()
	second := watch.NewRaceFreeFake()
	var opens atomic.Int32
	openedAgain := make(chan struct{})
	open := func(context.Context) (watch.Interface, error) {
		switch opens.Add(1) {
		case 1:
			return first, nil
		default:
			select {
			case <-openedAgain:
			default:
				close(openedAgain)
			}
			return second, nil
		}
	}
	go runEventWatch(ctx, "test", open, wake, time.Millisecond)
	first.Add(nil)
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("watch event did not wake reconciler")
	}
	first.Stop()
	select {
	case <-openedAgain:
	case <-time.After(time.Second):
		t.Fatal("closed watch was not reconnected")
	}
	cancel()
	second.Stop()
}

func TestCanceledEventWatchStopsWithoutOpening(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var opens atomic.Int32
	runEventWatch(ctx, "test", func(context.Context) (watch.Interface, error) {
		opens.Add(1)
		return watch.NewRaceFreeFake(), nil
	}, make(chan struct{}, 1), time.Millisecond)
	if opens.Load() != 0 {
		t.Fatalf("opened %d watches after cancellation", opens.Load())
	}
}
