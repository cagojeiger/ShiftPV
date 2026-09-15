package certificate

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// NameLabel, ManagedByLabel and ComponentLabel are the identity label keys the
// controller stamps on every resource it owns. Consumers that refuse to touch
// unmanaged copies of those resources must read the vocabulary from here.
const (
	NameLabel      = "app.kubernetes.io/name"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ComponentLabel = "app.kubernetes.io/component"
)

// NameValue, ManagedByValue and ValidationComponent are the identity label
// values that mark a resource as produced by this controller.
const (
	NameValue           = "shiftpv"
	ManagedByValue      = "shiftpv-controller"
	ValidationComponent = "lifecycle-admission"
)

const (
	caCertificateKey  = "ca.crt"
	caPrivateKeyKey   = "ca.key"
	tlsCertificateKey = corev1.TLSCertKey
	tlsPrivateKeyKey  = corev1.TLSPrivateKeyKey

	secretComponent          = "webhook-certificate"
	webhookComponent         = "mobility-admission"
	webhookName              = "mobility.shiftpv.io"
	validationWebhookName    = "lifecycle.shiftpv.io"
	validationCRWebhookName  = "lifecycle-crs.shiftpv.io"
	validationCRDWebhookName = "lifecycle-crds.shiftpv.io"
	protectedLabel           = "shiftpv.io/uninstall-protected"
)

type Config struct {
	Namespace                   string
	SecretName                  string
	ServiceName                 string
	ConfigurationName           string
	ValidationConfigurationName string
	OwnerCSIDriver              string
	AdmissionEnabled            bool
	Interval                    time.Duration
	ServingValidity             time.Duration
	ServingRenewBefore          time.Duration
	CAValidity                  time.Duration
	CARenewBefore               time.Duration
	Now                         func() time.Time
}

type ValidationGate interface {
	RunValidation(func() error) error
}

type Manager struct {
	Client         kubernetes.Interface
	Config         Config
	ValidationGate ValidationGate

	certificate atomic.Pointer[tls.Certificate]
}

func (m *Manager) Bootstrap(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	return m.Reconcile(ctx)
}

func (m *Manager) Run(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	ticker := time.NewTicker(m.Config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := m.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
				klog.Errorf("reconcile webhook certificate: %v", err)
			}
		}
	}
}

func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.GetCertificate,
	}
}

func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certificate := m.certificate.Load()
	if certificate == nil {
		return nil, fmt.Errorf("webhook serving certificate is not ready")
	}
	return certificate, nil
}

func (m *Manager) Reconcile(ctx context.Context) error {
	if err := m.validate(); err != nil {
		return err
	}
	service, err := m.Client.CoreV1().Services(m.Config.Namespace).Get(ctx, m.Config.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read webhook Service: %w", err)
	}
	driver, err := m.Client.StorageV1().CSIDrivers().Get(ctx, m.Config.OwnerCSIDriver, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read owner CSIDriver: %w", err)
	}
	trustedCA, err := m.currentTrustedCABundle(ctx, driver)
	if err != nil {
		return err
	}

	secret, err := m.Client.CoreV1().Secrets(m.Config.Namespace).Get(ctx, m.Config.SecretName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read webhook TLS Secret: %w", err)
	}
	if apierrors.IsNotFound(err) {
		secret = nil
	}
	now := m.Config.Now().UTC()
	current, currentErr := parseMaterial(secret, m.dnsNames(), now)
	oldCA := transitionBaseCA(secretData(secret, caCertificateKey), trustedCA, m.certificate.Load())
	rotateCA := currentErr != nil || current.ca.NotAfter.Before(now.Add(m.Config.CARenewBefore))
	rotateServing := !rotateCA && current.cert.Leaf.NotAfter.Before(now.Add(m.Config.ServingRenewBefore))

	next := current
	if rotateCA {
		next, err = generateMaterial(now, m.Config, m.dnsNames())
		if err != nil {
			return err
		}
		if bundle := mergeCertificateBundles(oldCA, next.caPEM); len(oldCA) > 0 && !bytes.Equal(bundle, next.caPEM) {
			if err := m.ensureWebhook(ctx, driver, bundle); err != nil {
				return fmt.Errorf("publish webhook CA transition bundle: %w", err)
			}
			if err := m.ensureValidation(ctx, driver, bundle); err != nil {
				return fmt.Errorf("publish validation webhook CA transition bundle: %w", err)
			}
		}
	} else if rotateServing {
		next, err = renewServing(now, m.Config, current, m.dnsNames())
		if err != nil {
			return err
		}
	}

	if err := m.ensureSecret(ctx, service, secret, next); err != nil {
		return err
	}
	m.certificate.Store(next.cert)
	if err := m.ensureWebhook(ctx, driver, next.caPEM); err != nil {
		return err
	}
	return m.ensureValidation(ctx, driver, next.caPEM)
}

func (m *Manager) validate() error {
	if m.Client == nil {
		return fmt.Errorf("Kubernetes client is required")
	}
	if m.Config.Namespace == "" || m.Config.SecretName == "" || m.Config.ServiceName == "" || m.Config.ConfigurationName == "" || m.Config.ValidationConfigurationName == "" || m.Config.OwnerCSIDriver == "" {
		return fmt.Errorf("webhook certificate resource names are required")
	}
	if m.Config.Interval <= 0 || m.Config.ServingValidity <= 0 || m.Config.ServingRenewBefore <= 0 || m.Config.CAValidity <= 0 || m.Config.CARenewBefore <= 0 {
		return fmt.Errorf("webhook certificate durations must be positive")
	}
	if m.Config.ServingRenewBefore >= m.Config.ServingValidity || m.Config.CARenewBefore >= m.Config.CAValidity {
		return fmt.Errorf("webhook certificate renewal windows must be shorter than validity")
	}
	if m.Config.Now == nil {
		return fmt.Errorf("webhook certificate clock is required")
	}
	return nil
}

func (m *Manager) dnsNames() []string {
	return []string{
		fmt.Sprintf("%s.%s.svc", m.Config.ServiceName, m.Config.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", m.Config.ServiceName, m.Config.Namespace),
	}
}

func (m *Manager) currentTrustedCABundle(ctx context.Context, owner *storagev1.CSIDriver) ([]byte, error) {
	configuration, err := m.Client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, m.Config.ConfigurationName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read current MutatingWebhookConfiguration trust: %w", err)
	}
	var bundles [][]byte
	if err == nil {
		if !managedResource(configuration, webhookComponent, "storage.k8s.io/v1", "CSIDriver", owner.Name, owner.UID) {
			return nil, fmt.Errorf("refuse to read trust from unmanaged MutatingWebhookConfiguration %q", configuration.Name)
		}
		for _, webhook := range configuration.Webhooks {
			if webhook.Name == webhookName {
				bundles = append(bundles, webhook.ClientConfig.CABundle)
			}
		}
	}

	validation, err := m.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, m.Config.ValidationConfigurationName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read current ValidatingWebhookConfiguration trust: %w", err)
	}
	if err == nil {
		if !managedResource(validation, ValidationComponent, "storage.k8s.io/v1", "CSIDriver", owner.Name, owner.UID) {
			return nil, fmt.Errorf("refuse to read trust from unmanaged ValidatingWebhookConfiguration %q", validation.Name)
		}
		for _, webhook := range validation.Webhooks {
			if webhook.Name == validationWebhookName {
				bundles = append(bundles, webhook.ClientConfig.CABundle)
			}
		}
	}
	return mergeCertificateBundles(bundles...), nil
}

func secretData(secret *corev1.Secret, key string) []byte {
	if secret == nil {
		return nil
	}
	return secret.Data[key]
}
