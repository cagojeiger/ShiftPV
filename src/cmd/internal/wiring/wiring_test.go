package wiring

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/rest"
)

func TestForConfigQualifiesFailureSteps(t *testing.T) {
	// A missing CA file makes both client constructors fail deterministically
	// without any network access.
	config := &rest.Config{Host: "https://example.invalid", TLSClientConfig: rest.TLSClientConfig{CAFile: "/nonexistent/ca.crt"}}
	for _, test := range []struct{ role, want string }{
		{"", "create Kubernetes client"},
		{"lifecycle admission", "create lifecycle admission Kubernetes client"},
	} {
		var steps []string
		clients := ForConfig(config, test.role, func(step string, err error) {
			if err == nil {
				t.Fatalf("%q reported without an error", step)
			}
			steps = append(steps, step)
		})
		if len(steps) != 1 || steps[0] != test.want {
			t.Fatalf("role %q reported %q, want [%q]", test.role, steps, test.want)
		}
		if clients != (Clients{}) {
			t.Fatalf("role %q returned clients after a failure: %+v", test.role, clients)
		}
	}
}

func TestForConfigBuildsBothClientsFromOneConfig(t *testing.T) {
	config := &rest.Config{Host: "https://example.invalid"}
	clients := ForConfig(config, "", func(step string, err error) {
		t.Fatalf("%s: %v", step, err)
	})
	if clients.Config != config || clients.Typed == nil || clients.Dynamic == nil {
		t.Fatalf("clients = %+v", clients)
	}
}

func TestBootstrapWithRetryRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	err := BootstrapWithRetry(context.Background(), "test bootstrap", func(context.Context) error {
		attempts++
		if attempts < 2 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("err = %v, attempts = %d", err, attempts)
	}
}

func TestBootstrapWithRetryStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := BootstrapWithRetry(ctx, "test bootstrap", func(context.Context) error {
		attempts++
		cancel()
		return errors.New("never")
	})
	if !errors.Is(err, context.Canceled) || attempts == 0 {
		t.Fatalf("err = %v, attempts = %d", err, attempts)
	}
}
