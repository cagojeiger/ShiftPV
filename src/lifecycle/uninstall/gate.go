package uninstall

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

type QuiesceGate struct {
	Store    *PermitStore
	Interval time.Duration

	validationMu sync.Mutex
	mu           sync.Mutex
	quiescing    bool
	active       int
}

func (g *QuiesceGate) Bootstrap(ctx context.Context) error {
	if g == nil || g.Store == nil || g.Interval <= 0 {
		return fmt.Errorf("uninstall quiesce gate is not configured")
	}
	return g.sync(ctx)
}

func (g *QuiesceGate) Run(ctx context.Context) error {
	if err := g.Bootstrap(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := g.sync(ctx); err != nil && ctx.Err() == nil {
				klog.Errorf("reconcile uninstall quiesce gate: %v", err)
			}
		}
	}
}

func (g *QuiesceGate) Enter() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.quiescing {
		return nil, fmt.Errorf("ShiftPV uninstall is quiescing new volume provisioning")
	}
	g.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			g.mu.Unlock()
		})
	}, nil
}

func (g *QuiesceGate) RunValidation(operation func() error) error {
	g.validationMu.Lock()
	defer g.validationMu.Unlock()

	g.mu.Lock()
	quiescing := g.quiescing
	g.mu.Unlock()
	if quiescing {
		return nil
	}
	return operation()
}

func (g *QuiesceGate) sync(ctx context.Context) error {
	attempt, quiescing, err := g.Store.Quiescing(ctx)
	if err != nil {
		return err
	}
	g.validationMu.Lock()
	g.mu.Lock()
	g.quiescing = quiescing
	ready := quiescing && g.active == 0
	g.mu.Unlock()
	g.validationMu.Unlock()
	if ready {
		return g.Store.Acknowledge(ctx, attempt)
	}
	return nil
}
