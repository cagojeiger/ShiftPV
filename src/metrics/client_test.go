package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestObservationClientBudgetAndInventory(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.UserAgent() != "shiftpv-metrics" {
			t.Errorf("user-agent %q", r.UserAgent())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`)
	}))
	defer server.Close()
	original := &rest.Config{Host: server.URL, QPS: 20, Burst: 40, RateLimiter: flowcontrol.NewTokenBucketRateLimiter(20, 40)}
	config := observationConfig(original)
	if config.QPS != 2 || config.Burst != 4 || config.Timeout != 10*time.Second || config.RateLimiter == original.RateLimiter {
		t.Fatal("metrics did not isolate budget")
	}
	if original.QPS != 20 || original.Timeout != 0 || original.UserAgent != "" {
		t.Fatal("storage config mutated")
	}
	c, err := New("metadata").NewController(original, "system", time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 5 {
		t.Fatalf("snapshot made %d API calls", calls)
	}
	if _, err := New().NewController(&rest.Config{Host: ":invalid"}, "system", time.Second, time.Minute); err == nil {
		t.Fatal("invalid metrics config accepted")
	}
}
