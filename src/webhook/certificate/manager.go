package certificate

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sync/atomic"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	caCertificateKey  = "ca.crt"
	caPrivateKeyKey   = "ca.key"
	tlsCertificateKey = corev1.TLSCertKey
	tlsPrivateKeyKey  = corev1.TLSPrivateKeyKey

	nameLabel             = "app.kubernetes.io/name"
	managedByLabel        = "app.kubernetes.io/managed-by"
	componentLabel        = "app.kubernetes.io/component"
	nameValue             = "shiftpv"
	managedByValue        = "shiftpv-controller"
	secretComponent       = "webhook-certificate"
	webhookComponent      = "mobility-admission"
	validationComponent   = "lifecycle-admission"
	webhookName           = "mobility.shiftpv.io"
	validationWebhookName = "lifecycle.shiftpv.io"
	protectedLabel        = "shiftpv.io/uninstall-protected"
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

type material struct {
	caPEM      []byte
	caKeyPEM   []byte
	certPEM    []byte
	certKeyPEM []byte
	ca         *x509.Certificate
	caKey      crypto.Signer
	cert       *tls.Certificate
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
		if !managedResource(validation, validationComponent, "storage.k8s.io/v1", "CSIDriver", owner.Name, owner.UID) {
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

func (m *Manager) ensureSecret(ctx context.Context, owner *corev1.Service, existing *corev1.Secret, value material) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Config.SecretName,
			Namespace: m.Config.Namespace,
			Labels: map[string]string{
				nameLabel:      nameValue,
				componentLabel: secretComponent,
				managedByLabel: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: owner.Name, UID: owner.UID}},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			caCertificateKey:  value.caPEM,
			caPrivateKeyKey:   value.caKeyPEM,
			tlsCertificateKey: value.certPEM,
			tlsPrivateKeyKey:  value.certKeyPEM,
		},
	}
	secrets := m.Client.CoreV1().Secrets(m.Config.Namespace)
	if existing == nil {
		if _, err := secrets.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create webhook TLS Secret: %w", err)
		}
		return nil
	}
	if !managedResource(existing, secretComponent, "v1", "Service", owner.Name, owner.UID) {
		return fmt.Errorf("refuse to update unmanaged webhook TLS Secret %q", existing.Name)
	}
	desired.ResourceVersion = existing.ResourceVersion
	if reflect.DeepEqual(existing.Type, desired.Type) && reflect.DeepEqual(existing.Data, desired.Data) && reflect.DeepEqual(existing.Labels, desired.Labels) && reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
		return nil
	}
	if _, err := secrets.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update webhook TLS Secret: %w", err)
	}
	return nil
}

func (m *Manager) ensureWebhook(ctx context.Context, owner *storagev1.CSIDriver, caBundle []byte) error {
	failurePolicy := admissionv1.Fail
	var matchConditions []admissionv1.MatchCondition
	if !m.Config.AdmissionEnabled {
		failurePolicy = admissionv1.Ignore
		matchConditions = []admissionv1.MatchCondition{{
			Name:       "mobility-disabled",
			Expression: "false",
		}}
	}
	matchPolicy := admissionv1.Equivalent
	sideEffects := admissionv1.SideEffectClassNone
	scope := admissionv1.NamespacedScope
	path := "/mutate"
	port := int32(443)
	timeout := int32(3)
	desired := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: m.Config.ConfigurationName,
			Labels: map[string]string{
				nameLabel:      nameValue,
				componentLabel: webhookComponent,
				managedByLabel: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "storage.k8s.io/v1", Kind: "CSIDriver", Name: owner.Name, UID: owner.UID}},
		},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    webhookName,
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &failurePolicy,
			MatchPolicy:             &matchPolicy,
			TimeoutSeconds:          &timeout,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service:  &admissionv1.ServiceReference{Namespace: m.Config.Namespace, Name: m.Config.ServiceName, Path: &path, Port: &port},
				CABundle: append([]byte(nil), caBundle...),
			},
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}, Scope: &scope},
			}},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"shiftpv.io/admission": "enabled"}},
			MatchConditions:   matchConditions,
		}},
	}
	webhooks := m.Client.AdmissionregistrationV1().MutatingWebhookConfigurations()
	existing, err := webhooks.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, createErr := webhooks.Create(ctx, desired, metav1.CreateOptions{}); createErr != nil {
			return fmt.Errorf("create MutatingWebhookConfiguration: %w", createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read MutatingWebhookConfiguration: %w", err)
	}
	if !managedResource(existing, webhookComponent, "storage.k8s.io/v1", "CSIDriver", owner.Name, owner.UID) {
		return fmt.Errorf("refuse to update unmanaged MutatingWebhookConfiguration %q", existing.Name)
	}
	desired.ResourceVersion = existing.ResourceVersion
	if reflect.DeepEqual(existing.Webhooks, desired.Webhooks) && reflect.DeepEqual(existing.Labels, desired.Labels) && reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
		return nil
	}
	if _, err := webhooks.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update MutatingWebhookConfiguration: %w", err)
	}
	return nil
}

func (m *Manager) ensureValidationWebhook(ctx context.Context, owner *storagev1.CSIDriver, caBundle []byte) error {
	failurePolicy := admissionv1.Fail
	matchPolicy := admissionv1.Equivalent
	sideEffects := admissionv1.SideEffectClassNone
	path := "/validate-delete"
	port := int32(443)
	timeout := int32(3)
	desired := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: m.Config.ValidationConfigurationName,
			Labels: map[string]string{
				nameLabel:      nameValue,
				componentLabel: validationComponent,
				managedByLabel: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "storage.k8s.io/v1", Kind: "CSIDriver", Name: owner.Name, UID: owner.UID}},
		},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    validationWebhookName,
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &failurePolicy,
			MatchPolicy:             &matchPolicy,
			TimeoutSeconds:          &timeout,
			ClientConfig: admissionv1.WebhookClientConfig{
				Service:  &admissionv1.ServiceReference{Namespace: m.Config.Namespace, Name: m.Config.ServiceName, Path: &path, Port: &port},
				CABundle: append([]byte(nil), caBundle...),
			},
			Rules: []admissionv1.RuleWithOperations{
				{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"services", "serviceaccounts"}}},
				{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments", "daemonsets"}}},
				{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{"rbac.authorization.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}}},
				{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{"storage.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"storageclasses", "csidrivers"}}},
			},
			ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{protectedLabel: "true"}},
		}},
	}
	webhooks := m.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	existing, err := webhooks.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, createErr := webhooks.Create(ctx, desired, metav1.CreateOptions{}); createErr != nil {
			return fmt.Errorf("create ValidatingWebhookConfiguration: %w", createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ValidatingWebhookConfiguration: %w", err)
	}
	if !managedResource(existing, validationComponent, "storage.k8s.io/v1", "CSIDriver", owner.Name, owner.UID) {
		return fmt.Errorf("refuse to update unmanaged ValidatingWebhookConfiguration %q", existing.Name)
	}
	desired.ResourceVersion = existing.ResourceVersion
	if reflect.DeepEqual(existing.Webhooks, desired.Webhooks) && reflect.DeepEqual(existing.Labels, desired.Labels) && reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) {
		return nil
	}
	if _, err := webhooks.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ValidatingWebhookConfiguration: %w", err)
	}
	return nil
}

func (m *Manager) ensureValidation(ctx context.Context, owner *storagev1.CSIDriver, caBundle []byte) error {
	operation := func() error { return m.ensureValidationWebhook(ctx, owner, caBundle) }
	if m.ValidationGate == nil {
		return operation()
	}
	return m.ValidationGate.RunValidation(operation)
}

func generateMaterial(now time.Time, config Config, dnsNames []string) (material, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return material{}, fmt.Errorf("generate webhook CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return material{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shiftpv-webhook-ca"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(config.CAValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		return material{}, fmt.Errorf("create webhook CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return material{}, fmt.Errorf("parse generated webhook CA certificate: %w", err)
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return material{}, fmt.Errorf("encode webhook CA key: %w", err)
	}
	base := material{
		caPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		caKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER}),
		ca:       ca,
		caKey:    caKey,
	}
	return renewServing(now, config, base, dnsNames)
}

func renewServing(now time.Time, config Config, current material, dnsNames []string) (material, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return material{}, fmt.Errorf("generate webhook serving key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return material{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     append([]string(nil), dnsNames...),
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(config.ServingValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, current.ca, key.Public(), current.caKey)
	if err != nil {
		return material{}, fmt.Errorf("create webhook serving certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return material{}, fmt.Errorf("encode webhook serving key: %w", err)
	}
	current.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	current.certKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(current.certPEM, current.certKeyPEM)
	if err != nil {
		return material{}, fmt.Errorf("load generated webhook serving certificate: %w", err)
	}
	pair.Leaf, err = x509.ParseCertificate(certDER)
	if err != nil {
		return material{}, fmt.Errorf("parse generated webhook serving certificate: %w", err)
	}
	current.cert = &pair
	return current, nil
}

func parseMaterial(secret *corev1.Secret, dnsNames []string, now time.Time) (material, error) {
	if secret == nil {
		return material{}, fmt.Errorf("webhook TLS Secret does not exist")
	}
	caBlock, _ := pem.Decode(secret.Data[caCertificateKey])
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return material{}, fmt.Errorf("webhook CA certificate is missing")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !ca.IsCA {
		return material{}, fmt.Errorf("webhook CA certificate is invalid")
	}
	caKey, err := parseSigner(secret.Data[caPrivateKeyKey])
	if err != nil {
		return material{}, err
	}
	if !publicKeysEqual(ca.PublicKey, caKey.Public()) {
		return material{}, fmt.Errorf("webhook CA key does not match certificate")
	}
	pair, err := tls.X509KeyPair(secret.Data[tlsCertificateKey], secret.Data[tlsPrivateKeyKey])
	if err != nil {
		return material{}, fmt.Errorf("load webhook serving certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return material{}, fmt.Errorf("webhook serving certificate chain is empty")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return material{}, fmt.Errorf("parse webhook serving certificate: %w", err)
	}
	if now.Before(pair.Leaf.NotBefore) || !now.Before(pair.Leaf.NotAfter) || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return material{}, fmt.Errorf("webhook certificate is outside its validity window")
	}
	if err := pair.Leaf.CheckSignatureFrom(ca); err != nil {
		return material{}, fmt.Errorf("verify webhook serving certificate issuer: %w", err)
	}
	for _, name := range dnsNames {
		if err := pair.Leaf.VerifyHostname(name); err != nil {
			return material{}, fmt.Errorf("verify webhook serving DNS name %q: %w", name, err)
		}
	}
	return material{
		caPEM:      append([]byte(nil), secret.Data[caCertificateKey]...),
		caKeyPEM:   append([]byte(nil), secret.Data[caPrivateKeyKey]...),
		certPEM:    append([]byte(nil), secret.Data[tlsCertificateKey]...),
		certKeyPEM: append([]byte(nil), secret.Data[tlsPrivateKeyKey]...),
		ca:         ca,
		caKey:      caKey,
		cert:       &pair,
	}, nil
}

func parseSigner(value []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(value)
	if block == nil {
		return nil, fmt.Errorf("webhook CA private key is missing")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse webhook CA private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("webhook CA private key cannot sign certificates")
	}
	return signer, nil
}

func publicKeysEqual(left, right crypto.PublicKey) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftDER, rightDER)
}

func managedResource(object metav1.Object, component, ownerAPIVersion, ownerKind, ownerName string, ownerUID types.UID) bool {
	labels := object.GetLabels()
	if labels[nameLabel] != nameValue || labels[componentLabel] != component || labels[managedByLabel] != managedByValue {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == ownerAPIVersion && owner.Kind == ownerKind && owner.Name == ownerName && owner.UID == ownerUID {
			return true
		}
	}
	return false
}

func mergeCertificateBundles(values ...[]byte) []byte {
	seen := map[string]struct{}{}
	var result []byte
	for _, value := range values {
		remaining := value
		for len(remaining) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil {
				break
			}
			remaining = rest
			if block.Type != "CERTIFICATE" {
				continue
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			key := string(certificate.Raw)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
		}
	}
	return result
}

func transitionBaseCA(secretCA, trustedCA []byte, active *tls.Certificate) []byte {
	if certificate := firstCertificate(secretCA, nil); certificate != nil {
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	}
	var activeLeaf *x509.Certificate
	if active != nil {
		activeLeaf = active.Leaf
		if activeLeaf == nil && len(active.Certificate) > 0 {
			activeLeaf, _ = x509.ParseCertificate(active.Certificate[0])
		}
	}
	certificate := firstCertificate(trustedCA, func(candidate *x509.Certificate) bool {
		return activeLeaf != nil && activeLeaf.CheckSignatureFrom(candidate) == nil
	})
	if certificate == nil {
		certificate = firstCertificate(trustedCA, nil)
	}
	if certificate == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

func firstCertificate(bundle []byte, accept func(*x509.Certificate) bool) *x509.Certificate {
	remaining := bundle
	for len(remaining) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			return nil
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err == nil && (accept == nil || accept(certificate)) {
			return certificate
		}
	}
	return nil
}

func secretData(secret *corev1.Secret, key string) []byte {
	if secret == nil {
		return nil
	}
	return secret.Data[key]
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
