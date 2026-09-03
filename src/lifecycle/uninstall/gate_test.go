package uninstall

import (
	"context"
	"testing"
	"time"
)

func TestQuiesceGateDrainsProvisioningAndSerializesValidation(t *testing.T) {
	store, _ := permitFixture()
	gate := &QuiesceGate{Store: store, Interval: time.Millisecond}
	if err := gate.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	leave, err := gate.Enter()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.BeginQuiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Enter(); err == nil {
		t.Fatal("provisioning entered after quiesce")
	}
	called := false
	if err := gate.RunValidation(func() error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("validation reconciliation ran after quiesce")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := store.WaitForQuiesced(waitCtx, attempt); err == nil {
		t.Fatal("controller acknowledged while provisioning was active")
	}
	leave()
	if err := gate.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.WaitForQuiesced(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
}

func TestQuiesceGateResumesAfterCancel(t *testing.T) {
	store, _ := permitFixture()
	gate := &QuiesceGate{Store: store, Interval: time.Millisecond}
	attempt, err := store.BeginQuiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Cancel(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := gate.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	leave, err := gate.Enter()
	if err != nil {
		t.Fatalf("provisioning did not resume: %v", err)
	}
	leave()
}

func TestQuiesceGateWaitsForValidationReconcileBeforeAcknowledging(t *testing.T) {
	store, _ := permitFixture()
	gate := &QuiesceGate{Store: store, Interval: time.Millisecond}
	if err := gate.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	validationDone := make(chan error, 1)
	go func() {
		validationDone <- gate.RunValidation(func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	attempt, err := store.BeginQuiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	syncDone := make(chan error, 1)
	go func() { syncDone <- gate.sync(context.Background()) }()
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := store.WaitForQuiesced(waitCtx, attempt); err == nil {
		t.Fatal("controller acknowledged while validation reconciliation was active")
	}

	close(release)
	if err := <-validationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-syncDone; err != nil {
		t.Fatal(err)
	}
	if err := store.WaitForQuiesced(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
}
