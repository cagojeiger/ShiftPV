package certificate

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestBootstrapCreatesTrustedCertificateResources(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	manager := &Manager{Client: client, Config: testConfig(&now)}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	secret, err := client.CoreV1().Secrets("shiftpv-system").Get(context.Background(), "shiftpv-webhook-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	for _, key := range []string{caCertificateKey, caPrivateKeyKey, tlsCertificateKey, tlsPrivateKeyKey} {
		if len(secret.Data[key]) == 0 {
			t.Fatalf("Secret key %q is empty", key)
		}
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != types.UID("service-uid") {
		t.Fatalf("Secret ownerReferences = %#v", secret.OwnerReferences)
	}

	configuration, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	if len(configuration.OwnerReferences) != 1 || configuration.OwnerReferences[0].UID != types.UID("driver-uid") {
		t.Fatalf("webhook ownerReferences = %#v", configuration.OwnerReferences)
	}
	if len(configuration.Webhooks) != 1 || !bytes.Equal(configuration.Webhooks[0].ClientConfig.CABundle, secret.Data[caCertificateKey]) {
		t.Fatalf("webhook CA bundle does not match Secret")
	}
	if got := configuration.Webhooks[0].ClientConfig.Service; got == nil || got.Name != "shiftpv-webhook" || got.Namespace != "shiftpv-system" {
		t.Fatalf("webhook service reference = %#v", got)
	}
	validation, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ValidatingWebhookConfiguration: %v", err)
	}
	if len(validation.Webhooks) != 1 || !bytes.Equal(validation.Webhooks[0].ClientConfig.CABundle, secret.Data[caCertificateKey]) {
		t.Fatal("validation webhook CA bundle does not match Secret")
	}
	if validation.Webhooks[0].ObjectSelector == nil || validation.Webhooks[0].ObjectSelector.MatchLabels[protectedLabel] != "true" {
		t.Fatalf("validation objectSelector = %#v", validation.Webhooks[0].ObjectSelector)
	}

	serving, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if err := serving.Leaf.VerifyHostname("shiftpv-webhook.shiftpv-system.svc"); err != nil {
		t.Fatalf("verify serving DNS name: %v", err)
	}
	if got := serving.Leaf.NotAfter.Sub(now); got != 90*24*time.Hour {
		t.Fatalf("serving certificate validity = %s, want 90 days", got)
	}
}

func TestReconcileIsIdempotentAndRenewsServingCertificate(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	manager := &Manager{Client: client, Config: testConfig(&now)}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	initial := mustSecret(t, client)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("idempotent Reconcile: %v", err)
	}
	unchanged := mustSecret(t, client)
	if !bytes.Equal(initial.Data[tlsCertificateKey], unchanged.Data[tlsCertificateKey]) {
		t.Fatal("fresh serving certificate was unexpectedly renewed")
	}

	leaf := parseCertificate(t, initial.Data[tlsCertificateKey])
	now = leaf.NotAfter.Add(-15 * 24 * time.Hour)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("renew Reconcile: %v", err)
	}
	renewed := mustSecret(t, client)
	if !bytes.Equal(initial.Data[caCertificateKey], renewed.Data[caCertificateKey]) {
		t.Fatal("serving renewal unexpectedly replaced the CA")
	}
	if bytes.Equal(initial.Data[tlsCertificateKey], renewed.Data[tlsCertificateKey]) {
		t.Fatal("expiring serving certificate was not renewed")
	}
	if got, err := manager.GetCertificate(nil); err != nil || bytes.Equal(got.Certificate[0], leaf.Raw) {
		t.Fatalf("hot-reloaded certificate = %#v, %v", got, err)
	}
}

func TestReconcileRecoversDeletedSecretAndRotatesCA(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	manager := &Manager{Client: client, Config: testConfig(&now)}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	initial := mustSecret(t, client)
	if err := client.CoreV1().Secrets("shiftpv-system").Delete(context.Background(), "shiftpv-webhook-tls", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete Secret: %v", err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("recover Reconcile: %v", err)
	}
	recovered := mustSecret(t, client)
	if bytes.Equal(initial.Data[caCertificateKey], recovered.Data[caCertificateKey]) {
		t.Fatal("deleted Secret was not replaced with new certificate material")
	}
	configuration, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	if !bytes.Equal(configuration.Webhooks[0].ClientConfig.CABundle, recovered.Data[caCertificateKey]) {
		t.Fatal("webhook did not converge to recovered CA")
	}
	validation, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{})
	if err != nil || !bytes.Equal(validation.Webhooks[0].ClientConfig.CABundle, recovered.Data[caCertificateKey]) {
		t.Fatalf("validation webhook did not converge to recovered CA: %v", err)
	}
}

func TestDeletedSecretRecoveryPublishesBothCAsBeforeSwitch(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	manager := &Manager{Client: client, Config: testConfig(&now)}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	initial := mustSecret(t, client)
	if err := client.CoreV1().Secrets("shiftpv-system").Delete(context.Background(), "shiftpv-webhook-tls", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete Secret: %v", err)
	}

	updates := 0
	client.PrependReactor("update", "mutatingwebhookconfigurations", func(clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 2 {
			return true, nil, errors.New("final CA convergence unavailable")
		}
		return false, nil, nil
	})
	if err := manager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "final CA convergence unavailable") {
		t.Fatalf("Reconcile error = %v", err)
	}
	recovered := mustSecret(t, client)
	configuration, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	wantBundle := mergeCertificateBundles(initial.Data[caCertificateKey], recovered.Data[caCertificateKey])
	if !bytes.Equal(configuration.Webhooks[0].ClientConfig.CABundle, wantBundle) {
		t.Fatal("failed final convergence did not leave both old and new CAs trusted")
	}
	validation, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{})
	if err != nil || !bytes.Equal(validation.Webhooks[0].ClientConfig.CABundle, wantBundle) {
		t.Fatalf("validation webhook did not retain both CAs: %v", err)
	}
	serving, err := manager.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if !bytes.Equal(serving.Certificate[0], parseCertificate(t, recovered.Data[tlsCertificateKey]).Raw) {
		t.Fatal("hot-reloaded certificate does not match the CA transition Secret")
	}
}

func TestReconcileRotatesExpiringCA(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	config := testConfig(&now)
	config.CAValidity = 180 * 24 * time.Hour
	config.CARenewBefore = 60 * 24 * time.Hour
	config.ServingValidity = 170 * 24 * time.Hour
	config.ServingRenewBefore = 10 * 24 * time.Hour
	manager := &Manager{Client: client, Config: config}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	initial := mustSecret(t, client)
	now = parseCertificate(t, initial.Data[caCertificateKey]).NotAfter.Add(-30 * 24 * time.Hour)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("rotate CA Reconcile: %v", err)
	}
	rotated := mustSecret(t, client)
	if bytes.Equal(initial.Data[caCertificateKey], rotated.Data[caCertificateKey]) {
		t.Fatal("expiring CA was not rotated")
	}
	configuration, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get MutatingWebhookConfiguration: %v", err)
	}
	if !bytes.Equal(configuration.Webhooks[0].ClientConfig.CABundle, rotated.Data[caCertificateKey]) {
		t.Fatal("webhook CA bundle did not converge to rotated CA")
	}
}

func TestRepeatedCATransitionFailureKeepsBundleBounded(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	config := testConfig(&now)
	config.CAValidity = 180 * 24 * time.Hour
	config.CARenewBefore = 60 * 24 * time.Hour
	config.ServingValidity = 170 * 24 * time.Hour
	config.ServingRenewBefore = 10 * 24 * time.Hour
	manager := &Manager{Client: client, Config: config}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	initial := mustSecret(t, client)
	now = parseCertificate(t, initial.Data[caCertificateKey]).NotAfter.Add(-30 * 24 * time.Hour)
	client.PrependReactor("update", "validatingwebhookconfigurations", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("validation transition unavailable")
	})

	var firstBundle []byte
	for attempt := 0; attempt < 2; attempt++ {
		err := manager.Reconcile(context.Background())
		if err == nil || !strings.Contains(err.Error(), "validation transition unavailable") {
			t.Fatalf("Reconcile attempt %d error = %v", attempt+1, err)
		}
		configuration, getErr := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("get MutatingWebhookConfiguration: %v", getErr)
		}
		bundle := configuration.Webhooks[0].ClientConfig.CABundle
		if got := certificateCount(bundle); got != 2 {
			t.Fatalf("transition bundle after attempt %d has %d certificates, want 2", attempt+1, got)
		}
		if !bytes.Contains(bundle, initial.Data[caCertificateKey]) {
			t.Fatalf("transition bundle after attempt %d lost the active CA", attempt+1)
		}
		if attempt == 0 {
			firstBundle = append([]byte(nil), bundle...)
		} else if bytes.Equal(firstBundle, bundle) {
			t.Fatal("retry unexpectedly reused unpersisted candidate CA")
		}
	}
	if !bytes.Equal(mustSecret(t, client).Data[caCertificateKey], initial.Data[caCertificateKey]) {
		t.Fatal("failed transition changed the persisted CA")
	}
}

func TestRunRenewsOnInterval(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	config := testConfig(&now)
	config.Interval = 5 * time.Millisecond
	manager := &Manager{Client: client, Config: config}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	initial := mustSecret(t, client)
	now = parseCertificate(t, initial.Data[tlsCertificateKey]).NotAfter.Add(-15 * 24 * time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !bytes.Equal(initial.Data[tlsCertificateKey], mustSecret(t, client).Data[tlsCertificateKey]) {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("certificate was not renewed by the periodic reconciler")
}

func TestReconcileDisablesAndReenablesAdmissionWithoutDeletingResources(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	config := testConfig(&now)
	config.AdmissionEnabled = false
	manager := &Manager{Client: client, Config: config}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := client.CoreV1().Secrets("shiftpv-system").Get(context.Background(), "shiftpv-webhook-tls", metav1.GetOptions{}); err != nil {
		t.Fatalf("disabled admission Secret: %v", err)
	}
	configuration, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("disabled admission configuration: %v", err)
	}
	webhook := configuration.Webhooks[0]
	if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionv1.Ignore {
		t.Fatalf("disabled failurePolicy = %v, want Ignore", webhook.FailurePolicy)
	}
	if len(webhook.MatchConditions) != 1 || webhook.MatchConditions[0].Expression != "false" {
		t.Fatalf("disabled matchConditions = %#v", webhook.MatchConditions)
	}

	manager.Config.AdmissionEnabled = true
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("reenable Reconcile: %v", err)
	}
	configuration, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reenabled admission configuration: %v", err)
	}
	webhook = configuration.Webhooks[0]
	if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionv1.Fail {
		t.Fatalf("enabled failurePolicy = %v, want Fail", webhook.FailurePolicy)
	}
	if len(webhook.MatchConditions) != 0 {
		t.Fatalf("enabled matchConditions = %#v, want none", webhook.MatchConditions)
	}
	validation, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lifecycle validation configuration: %v", err)
	}
	if validation.Webhooks[0].FailurePolicy == nil || *validation.Webhooks[0].FailurePolicy != admissionv1.Fail {
		t.Fatalf("lifecycle validation failurePolicy = %v, want Fail", validation.Webhooks[0].FailurePolicy)
	}
}

func TestReconcileRefusesUnmanagedResources(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	if _, err := client.CoreV1().Secrets("shiftpv-system").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shiftpv-webhook-tls", Namespace: "shiftpv-system", UID: types.UID("external-secret")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create unmanaged Secret: %v", err)
	}
	manager := &Manager{Client: client, Config: testConfig(&now)}
	if err := manager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "unmanaged webhook TLS Secret") {
		t.Fatalf("Reconcile error = %v", err)
	}

	webhookClient := certificateClient()
	if _, err := webhookClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.Background(), &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "shiftpv-mobility", UID: types.UID("external-webhook")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create unmanaged MutatingWebhookConfiguration: %v", err)
	}
	webhookManager := &Manager{Client: webhookClient, Config: testConfig(&now)}
	if err := webhookManager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "unmanaged MutatingWebhookConfiguration") {
		t.Fatalf("Reconcile error = %v", err)
	}
	if _, err := webhookClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "shiftpv-mobility", metav1.GetOptions{}); err != nil {
		t.Fatalf("unmanaged MutatingWebhookConfiguration was modified or deleted: %v", err)
	}
}

func TestReconcileRefusesManagedLabelsWithWrongOwnerUID(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	manager := &Manager{Client: client, Config: testConfig(&now)}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	secret := mustSecret(t, client)
	secret.OwnerReferences[0].UID = types.UID("stale-service")
	if _, err := client.CoreV1().Secrets(secret.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "unmanaged webhook TLS Secret") {
		t.Fatalf("wrong owner UID was accepted: %v", err)
	}
}

func TestValidationReconcileStopsWhileQuiescing(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	client := certificateClient()
	gate := &testValidationGate{quiescing: true}
	manager := &Manager{Client: client, Config: testConfig(&now), ValidationGate: gate}
	if err := manager.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{}); err == nil {
		t.Fatal("validation webhook was created while quiescing")
	}
	gate.quiescing = false
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "shiftpv-lifecycle", metav1.GetOptions{}); err != nil {
		t.Fatalf("validation webhook did not recover after quiesce: %v", err)
	}
}

type testValidationGate struct {
	quiescing bool
}

func (g *testValidationGate) RunValidation(operation func() error) error {
	if g.quiescing {
		return nil
	}
	return operation()
}

func certificateClient() *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "shiftpv-webhook", Namespace: "shiftpv-system", UID: types.UID("service-uid")}},
		&storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: "csi.shiftpv.io", UID: types.UID("driver-uid")}},
	)
}

func testConfig(now *time.Time) Config {
	return Config{
		Namespace:                   "shiftpv-system",
		SecretName:                  "shiftpv-webhook-tls",
		ServiceName:                 "shiftpv-webhook",
		ConfigurationName:           "shiftpv-mobility",
		ValidationConfigurationName: "shiftpv-lifecycle",
		OwnerCSIDriver:              "csi.shiftpv.io",
		AdmissionEnabled:            true,
		Interval:                    time.Minute,
		ServingValidity:             90 * 24 * time.Hour,
		ServingRenewBefore:          30 * 24 * time.Hour,
		CAValidity:                  10 * 365 * 24 * time.Hour,
		CARenewBefore:               365 * 24 * time.Hour,
		Now:                         func() time.Time { return *now },
	}
}

func mustSecret(t *testing.T, client *fake.Clientset) *corev1.Secret {
	t.Helper()
	secret, err := client.CoreV1().Secrets("shiftpv-system").Get(context.Background(), "shiftpv-webhook-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Secret: %v", err)
	}
	return secret
}

func parseCertificate(t *testing.T, value []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(value)
	if block == nil {
		t.Fatal("certificate PEM is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certificate
}

func certificateCount(bundle []byte) int {
	count := 0
	remaining := bundle
	for len(remaining) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = rest
		if block.Type == "CERTIFICATE" {
			count++
		}
	}
	return count
}
