package uninstall

import (
	"context"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPermitRequiresAcknowledgedQuiesce(t *testing.T) {
	store, _ := permitFixture()
	attempt, err := store.BeginQuiesce(context.Background())
	if err != nil {
		t.Fatalf("BeginQuiesce: %v", err)
	}
	if granted, err := store.Granted(context.Background()); err != nil || granted {
		t.Fatalf("Granted before acknowledgement = %v, %v", granted, err)
	}
	if err := store.Grant(context.Background(), attempt); err == nil {
		t.Fatal("Grant succeeded before controller acknowledgement")
	}
	if err := store.Acknowledge(context.Background(), attempt); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if err := store.WaitForQuiesced(context.Background(), attempt); err != nil {
		t.Fatalf("WaitForQuiesced: %v", err)
	}
	if err := store.Grant(context.Background(), attempt); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if granted, err := store.Granted(context.Background()); err != nil || !granted {
		t.Fatalf("Granted = %v, %v", granted, err)
	}
}

func TestPermitCancelOnlyRemovesCurrentAttempt(t *testing.T) {
	store, client := permitFixture()
	attempt, err := store.BeginQuiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Cancel(context.Background(), "another-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps(store.Namespace).Get(context.Background(), store.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("current state was removed by a stale attempt: %v", err)
	}
	if err := store.Cancel(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if currentAttempt, quiescing, err := store.Quiescing(context.Background()); err != nil || quiescing || currentAttempt != "" {
		t.Fatalf("Quiescing after Cancel = %q, %v, %v", currentAttempt, quiescing, err)
	}
}

func TestPermitStoreDisablesOnlyManagedValidation(t *testing.T) {
	store, client := permitFixture()
	driver, _ := client.StorageV1().CSIDrivers().Get(context.Background(), DriverName, metav1.GetOptions{})
	managed := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
		Name: "shiftpv-lifecycle", UID: types.UID("webhook-uid"),
		Labels:          map[string]string{managedNameLabel: "shiftpv", managedByLabel: "shiftpv-controller", managedComponentLabel: "lifecycle-admission"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: storagev1.SchemeGroupVersion.String(), Kind: "CSIDriver", Name: DriverName, UID: driver.UID}},
	}}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.Background(), managed, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.DisableValidation(context.Background(), managed.Name); err != nil {
		t.Fatalf("DisableValidation: %v", err)
	}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), managed.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("managed validation configuration still exists")
	}

	unmanaged := managed.DeepCopy()
	unmanaged.ResourceVersion = ""
	unmanaged.UID = types.UID("external")
	unmanaged.OwnerReferences[0].UID = types.UID("wrong-driver")
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.Background(), unmanaged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.DisableValidation(context.Background(), unmanaged.Name); err == nil {
		t.Fatal("wrong-owner validation configuration was accepted")
	}
}

func TestPermitFailsClosedForMalformedExpiredAndRecreatedOwner(t *testing.T) {
	now := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	store, client := permitFixture()
	store.Now = func() time.Time { return now }
	attempt, err := store.BeginQuiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Acknowledge(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.Grant(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	now = now.Add(permitTTL + time.Second)
	if granted, err := store.Granted(context.Background()); err != nil || granted {
		t.Fatalf("expired Granted = %v, %v", granted, err)
	}

	now = now.Add(-permitTTL)
	if err := client.StorageV1().CSIDrivers().Delete(context.Background(), DriverName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StorageV1().CSIDrivers().Create(context.Background(), &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: DriverName, UID: types.UID("new-driver")}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if granted, err := store.Granted(context.Background()); err == nil || granted {
		t.Fatalf("recreated owner Granted = %v, %v", granted, err)
	}

	malformed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: store.Namespace}}
	if _, err := client.CoreV1().ConfigMaps(store.Namespace).Create(context.Background(), malformed, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	bad := &PermitStore{Client: client, Namespace: store.Namespace, Name: malformed.Name, CSIDriver: DriverName}
	if granted, err := bad.Granted(context.Background()); err == nil || granted {
		t.Fatalf("malformed Granted = %v, %v", granted, err)
	}
}

func permitFixture() (*PermitStore, *fake.Clientset) {
	client := fake.NewSimpleClientset(&storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: DriverName, UID: types.UID("driver-uid")}})
	return &PermitStore{Client: client, Namespace: "shiftpv-system", Name: "shiftpv-uninstall-permit", CSIDriver: DriverName}, client
}
