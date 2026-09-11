package metrics

import (
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
)

func observationConfig(config *rest.Config) *rest.Config {
	copy := rest.CopyConfig(config)
	copy.QPS, copy.Burst = 2, 4
	copy.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(copy.QPS, copy.Burst)
	copy.Timeout = 10 * time.Second
	copy.UserAgent = "shiftpv-metrics"
	return copy
}

// NewController isolates observation's rate limiter from provisioning and mobility.
func (e *Exporter) NewController(config *rest.Config, namespace string, interval, staleAfter time.Duration) (*Controller, error) {
	observation := observationConfig(config)
	client, err := kubernetes.NewForConfig(observation)
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(observation)
	if err != nil {
		return nil, err
	}
	return &Controller{Exporter: e, Client: client, Inventory: &volumeapi.Registry{Client: dynamicClient}, Cleanups: &cleanupapi.Store{Client: dynamicClient}, Namespace: namespace, Interval: interval, StaleAfter: staleAfter}, nil
}
