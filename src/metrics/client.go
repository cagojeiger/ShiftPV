package metrics

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/project-jelly/ShiftPV/src/kubernetes/cleanupapi"
	"github.com/project-jelly/ShiftPV/src/kubernetes/volumeapi"
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
func (e *Exporter) NewController(config *rest.Config, interval, staleAfter time.Duration) (*Controller, error) {
	observation := observationConfig(config)
	dynamicClient, err := dynamic.NewForConfig(observation)
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(observation)
	if err != nil {
		return nil, err
	}
	return &Controller{Exporter: e, Inventory: &volumeapi.Registry{Client: dynamicClient}, Cleanups: &cleanupapi.Store{Client: dynamicClient}, PersistentVolumes: persistentVolumeStore{Client: client}, Interval: interval, StaleAfter: staleAfter}, nil
}

// persistentVolumeListPageSize bounds one cluster-wide PersistentVolume
// response so a large installation cannot make observation allocate the whole
// collection at once.
const persistentVolumeListPageSize = 500

// persistentVolumeStore lists every PersistentVolume; the sample builder keeps
// only the ones this driver provisioned.
//
// The first page asks for resourceVersion "0", which the API server answers
// from its watch cache instead of a quorum read against etcd. Observation runs
// cluster-wide on every interval and must not cost a quorum read each time; the
// reclaim decision this feeds is a six-hour alert, so a slightly stale view is
// the right trade. Continuation pages carry no resourceVersion because a
// continue token already pins the snapshot they page through.
type persistentVolumeStore struct{ Client kubernetes.Interface }

func (s persistentVolumeStore) List(ctx context.Context) ([]corev1.PersistentVolume, error) {
	options := metav1.ListOptions{ResourceVersion: "0", Limit: persistentVolumeListPageSize}
	var persistentVolumes []corev1.PersistentVolume
	for {
		list, err := s.Client.CoreV1().PersistentVolumes().List(ctx, options)
		if err != nil {
			return nil, err
		}
		persistentVolumes = append(persistentVolumes, list.Items...)
		if list.Continue == "" {
			return persistentVolumes, nil
		}
		options.Continue, options.ResourceVersion = list.Continue, ""
	}
}
