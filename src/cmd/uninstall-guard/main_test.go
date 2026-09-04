package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cagojeiger/ShiftPV/src/kubernetes/volumeapi"
	uninstallcheck "github.com/cagojeiger/ShiftPV/src/lifecycle/uninstall"
)

type emptyVolumeRepository struct{}

func (emptyVolumeRepository) ListVolumes(context.Context) (map[string]volumeapi.State, error) {
	return map[string]volumeapi.State{}, nil
}
func (emptyVolumeRepository) ListMoves(context.Context) ([]volumeapi.Move, error) {
	return nil, nil
}

func TestRunCompletesQuiescedTeardown(t *testing.T) {
	client := uninstallClient()
	store := &uninstallcheck.PermitStore{Client: client, Namespace: "shiftpv-system", Name: "shiftpv-uninstall-permit", CSIDriver: uninstallcheck.DriverName}
	gate := &uninstallcheck.QuiesceGate{Store: store, Interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _ = gate.Run(ctx) }()
	checker := &uninstallcheck.Checker{Client: client, Volumes: emptyVolumeRepository{}, StorageClassName: "shiftpv"}
	if err := run(ctx, checker, store, "shiftpv-lifecycle"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if granted, err := store.Granted(context.Background()); err != nil || !granted {
		t.Fatalf("Granted = %v, %v", granted, err)
	}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{}); err == nil {
		t.Fatal("lifecycle validation still exists")
	}
}

func TestRunCancelsQuiesceWhenDependenciesExist(t *testing.T) {
	client := uninstallClient()
	client.Tracker().Add(&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-data"}, Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: uninstallcheck.DriverName, VolumeHandle: "volume"}},
	}})
	store := &uninstallcheck.PermitStore{Client: client, Namespace: "shiftpv-system", Name: "shiftpv-uninstall-permit", CSIDriver: uninstallcheck.DriverName}
	gate := &uninstallcheck.QuiesceGate{Store: store, Interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _ = gate.Run(ctx) }()
	checker := &uninstallcheck.Checker{Client: client, Volumes: emptyVolumeRepository{}, StorageClassName: "shiftpv"}
	err := run(ctx, checker, store, "shiftpv-lifecycle")
	if err == nil || !strings.Contains(err.Error(), "PersistentVolume pv-data") {
		t.Fatalf("run error = %v", err)
	}
	if _, quiescing, err := store.Quiescing(context.Background()); err != nil || quiescing {
		t.Fatalf("quiesce after failed run = %v, %v", quiescing, err)
	}
}

func TestRunCancelsQuiesceWhenValidationRemovalFails(t *testing.T) {
	client := uninstallClient()
	client.PrependReactor("delete", "validatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected validation delete failure")
	})
	store := &uninstallcheck.PermitStore{Client: client, Namespace: "shiftpv-system", Name: "shiftpv-uninstall-permit", CSIDriver: uninstallcheck.DriverName}
	gate := &uninstallcheck.QuiesceGate{Store: store, Interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _ = gate.Run(ctx) }()
	checker := &uninstallcheck.Checker{Client: client, Volumes: emptyVolumeRepository{}, StorageClassName: "shiftpv"}
	err := run(ctx, checker, store, "shiftpv-lifecycle")
	if err == nil || !strings.Contains(err.Error(), "injected validation delete failure") {
		t.Fatalf("run error = %v", err)
	}
	if _, quiescing, err := store.Quiescing(context.Background()); err != nil || quiescing {
		t.Fatalf("quiesce after validation failure = %v, %v", quiescing, err)
	}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{}); err != nil {
		t.Fatalf("validation configuration was lost: %v", err)
	}
}

func TestRunWithRetryCompletesAfterBlockerIsRemoved(t *testing.T) {
	client := uninstallClient()
	blocker := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-data"}, Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: uninstallcheck.DriverName, VolumeHandle: "volume"}},
	}}
	if err := client.Tracker().Add(blocker); err != nil {
		t.Fatal(err)
	}
	checks := 0
	client.PrependReactor("list", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		checks++
		if checks != 1 {
			return false, nil, nil
		}
		// The first snapshot must contain the blocker. Remove it for the next
		// attempt instead of racing a 20ms sleep against the 200ms ACK poll.
		err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumes"), "", blocker.Name)
		return true, &corev1.PersistentVolumeList{Items: []corev1.PersistentVolume{*blocker.DeepCopy()}}, err
	})
	store := &uninstallcheck.PermitStore{Client: client, Namespace: "shiftpv-system", Name: "shiftpv-uninstall-permit", CSIDriver: uninstallcheck.DriverName}
	gate := &uninstallcheck.QuiesceGate{Store: store, Interval: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = gate.Run(ctx) }()
	checker := &uninstallcheck.Checker{Client: client, Volumes: emptyVolumeRepository{}, StorageClassName: "shiftpv"}

	if err := runWithRetry(ctx, checker, store, "shiftpv-lifecycle", time.Second, time.Millisecond); err != nil {
		t.Fatalf("runWithRetry: %v", err)
	}
	if checks < 2 {
		t.Fatal("retry did not observe both blocked and clear snapshots")
	}
	if granted, err := store.Granted(context.Background()); err != nil || !granted {
		t.Fatalf("Granted = %v, %v", granted, err)
	}
}

func TestRunWithRetryValidatesDurationsAndStops(t *testing.T) {
	client := uninstallClient()
	store := &uninstallcheck.PermitStore{Client: client, Namespace: "shiftpv-system", Name: "shiftpv-uninstall-permit", CSIDriver: uninstallcheck.DriverName}
	checker := &uninstallcheck.Checker{Client: client, Volumes: emptyVolumeRepository{}, StorageClassName: "shiftpv"}
	if err := runWithRetry(context.Background(), checker, store, "shiftpv-lifecycle", 0, time.Millisecond); err == nil {
		t.Fatal("zero attempt timeout was accepted")
	}
	if err := runWithRetry(context.Background(), checker, store, "shiftpv-lifecycle", time.Second, 0); err == nil {
		t.Fatal("zero retry interval was accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runWithRetry(ctx, checker, store, "shiftpv-lifecycle", time.Second, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "retry interrupted") {
		t.Fatalf("canceled retry error = %v", err)
	}
}

func uninstallClient() *fake.Clientset {
	driver := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: uninstallcheck.DriverName, UID: types.UID("driver-uid")}}
	validation := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
		Name: "shiftpv-lifecycle", UID: types.UID("validation-uid"),
		Labels: map[string]string{
			"app.kubernetes.io/name": "shiftpv", "app.kubernetes.io/managed-by": "shiftpv-controller", "app.kubernetes.io/component": "lifecycle-admission",
		},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: storagev1.SchemeGroupVersion.String(), Kind: "CSIDriver", Name: driver.Name, UID: driver.UID}},
	}}
	return fake.NewSimpleClientset(driver, validation)
}
