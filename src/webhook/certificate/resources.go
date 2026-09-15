package certificate

import (
	"context"
	"fmt"
	"reflect"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ManagedLabels returns the identity labels the controller stamps on every
// resource it owns for the given component.
func ManagedLabels(component string) map[string]string {
	return map[string]string{
		NameLabel:      NameValue,
		ComponentLabel: component,
		ManagedByLabel: ManagedByValue,
	}
}

type managedOwner struct {
	apiVersion string
	kind       string
	name       string
	uid        types.UID
}

// ensureRequest describes one managed resource reconciliation: how to read the
// current object, how to write the desired object, and how to decide that the
// two already agree.
type ensureRequest[T metav1.Object] struct {
	resource  string
	component string
	owner     managedOwner
	desired   T
	get       func(context.Context) (T, error)
	create    func(context.Context, T) error
	update    func(context.Context, T) error
	equal     func(existing, desired T) bool
}

// ensureManaged converges one ShiftPV-owned resource onto its desired shape. It
// creates the object when it is absent, refuses to touch an object that ShiftPV
// does not own, and skips the update when the live object already matches.
func ensureManaged[T metav1.Object](ctx context.Context, request ensureRequest[T]) error {
	existing, err := request.get(ctx)
	if apierrors.IsNotFound(err) {
		if createErr := request.create(ctx, request.desired); createErr != nil {
			return fmt.Errorf("create %s: %w", request.resource, createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", request.resource, err)
	}
	owner := request.owner
	if !managedResource(existing, request.component, owner.apiVersion, owner.kind, owner.name, owner.uid) {
		return fmt.Errorf("refuse to update unmanaged %s %q", request.resource, existing.GetName())
	}
	request.desired.SetResourceVersion(existing.GetResourceVersion())
	if request.equal(existing, request.desired) {
		return nil
	}
	if updateErr := request.update(ctx, request.desired); updateErr != nil {
		return fmt.Errorf("update %s: %w", request.resource, updateErr)
	}
	return nil
}

func managedResource(object metav1.Object, component, ownerAPIVersion, ownerKind, ownerName string, ownerUID types.UID) bool {
	labels := object.GetLabels()
	if labels[NameLabel] != NameValue || labels[ComponentLabel] != component || labels[ManagedByLabel] != ManagedByValue {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion == ownerAPIVersion && owner.Kind == ownerKind && owner.Name == ownerName && owner.UID == ownerUID {
			return true
		}
	}
	return false
}

func (m *Manager) ensureSecret(ctx context.Context, owner *corev1.Service, existing *corev1.Secret, value material) error {
	secrets := m.Client.CoreV1().Secrets(m.Config.Namespace)
	return ensureManaged(ctx, ensureRequest[*corev1.Secret]{
		resource:  "webhook TLS Secret",
		component: secretComponent,
		owner:     managedOwner{apiVersion: "v1", kind: "Service", name: owner.Name, uid: owner.UID},
		desired:   m.desiredSecret(owner, value),
		get: func(context.Context) (*corev1.Secret, error) {
			if existing == nil {
				return nil, apierrors.NewNotFound(corev1.Resource("secrets"), m.Config.SecretName)
			}
			return existing, nil
		},
		create: func(ctx context.Context, desired *corev1.Secret) error {
			_, err := secrets.Create(ctx, desired, metav1.CreateOptions{})
			return err
		},
		update: func(ctx context.Context, desired *corev1.Secret) error {
			_, err := secrets.Update(ctx, desired, metav1.UpdateOptions{})
			return err
		},
		equal: func(existing, desired *corev1.Secret) bool {
			return reflect.DeepEqual(existing.Type, desired.Type) &&
				reflect.DeepEqual(existing.Data, desired.Data) &&
				reflect.DeepEqual(existing.Labels, desired.Labels) &&
				reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences)
		},
	})
}

func (m *Manager) ensureWebhook(ctx context.Context, owner *storagev1.CSIDriver, caBundle []byte) error {
	webhooks := m.Client.AdmissionregistrationV1().MutatingWebhookConfigurations()
	desired := m.desiredWebhook(owner, caBundle)
	return ensureManaged(ctx, ensureRequest[*admissionv1.MutatingWebhookConfiguration]{
		resource:  "MutatingWebhookConfiguration",
		component: webhookComponent,
		owner:     csiDriverOwner(owner),
		desired:   desired,
		get: func(ctx context.Context) (*admissionv1.MutatingWebhookConfiguration, error) {
			return webhooks.Get(ctx, desired.Name, metav1.GetOptions{})
		},
		create: func(ctx context.Context, desired *admissionv1.MutatingWebhookConfiguration) error {
			_, err := webhooks.Create(ctx, desired, metav1.CreateOptions{})
			return err
		},
		update: func(ctx context.Context, desired *admissionv1.MutatingWebhookConfiguration) error {
			_, err := webhooks.Update(ctx, desired, metav1.UpdateOptions{})
			return err
		},
		equal: func(existing, desired *admissionv1.MutatingWebhookConfiguration) bool {
			return reflect.DeepEqual(existing.Webhooks, desired.Webhooks) &&
				reflect.DeepEqual(existing.Labels, desired.Labels) &&
				reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences)
		},
	})
}

func (m *Manager) ensureValidationWebhook(ctx context.Context, owner *storagev1.CSIDriver, caBundle []byte) error {
	webhooks := m.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	desired := m.desiredValidationWebhook(owner, caBundle)
	return ensureManaged(ctx, ensureRequest[*admissionv1.ValidatingWebhookConfiguration]{
		resource:  "ValidatingWebhookConfiguration",
		component: ValidationComponent,
		owner:     csiDriverOwner(owner),
		desired:   desired,
		get: func(ctx context.Context) (*admissionv1.ValidatingWebhookConfiguration, error) {
			return webhooks.Get(ctx, desired.Name, metav1.GetOptions{})
		},
		create: func(ctx context.Context, desired *admissionv1.ValidatingWebhookConfiguration) error {
			_, err := webhooks.Create(ctx, desired, metav1.CreateOptions{})
			return err
		},
		update: func(ctx context.Context, desired *admissionv1.ValidatingWebhookConfiguration) error {
			_, err := webhooks.Update(ctx, desired, metav1.UpdateOptions{})
			return err
		},
		equal: func(existing, desired *admissionv1.ValidatingWebhookConfiguration) bool {
			return reflect.DeepEqual(existing.Webhooks, desired.Webhooks) &&
				reflect.DeepEqual(existing.Labels, desired.Labels) &&
				reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences)
		},
	})
}

func (m *Manager) ensureValidation(ctx context.Context, owner *storagev1.CSIDriver, caBundle []byte) error {
	operation := func() error { return m.ensureValidationWebhook(ctx, owner, caBundle) }
	if m.ValidationGate == nil {
		return operation()
	}
	return m.ValidationGate.RunValidation(operation)
}

func (m *Manager) desiredSecret(owner *corev1.Service, value material) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            m.Config.SecretName,
			Namespace:       m.Config.Namespace,
			Labels:          ManagedLabels(secretComponent),
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
}

func (m *Manager) desiredWebhook(owner *storagev1.CSIDriver, caBundle []byte) *admissionv1.MutatingWebhookConfiguration {
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
	timeout := int32(3)
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            m.Config.ConfigurationName,
			Labels:          ManagedLabels(webhookComponent),
			OwnerReferences: []metav1.OwnerReference{ownerReference(owner)},
		},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    webhookName,
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			FailurePolicy:           &failurePolicy,
			MatchPolicy:             &matchPolicy,
			TimeoutSeconds:          &timeout,
			ClientConfig:            m.clientConfig("/mutate", caBundle),
			Rules: []admissionv1.RuleWithOperations{{
				Operations: []admissionv1.OperationType{admissionv1.Create},
				Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}, Scope: &scope},
			}},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"shiftpv.io/admission": "enabled"}},
			MatchConditions:   matchConditions,
		}},
	}
}

func (m *Manager) desiredValidationWebhook(owner *storagev1.CSIDriver, caBundle []byte) *admissionv1.ValidatingWebhookConfiguration {
	failurePolicy := admissionv1.Fail
	matchPolicy := admissionv1.Equivalent
	sideEffects := admissionv1.SideEffectClassNone
	timeout := int32(3)
	return &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            m.Config.ValidationConfigurationName,
			Labels:          ManagedLabels(ValidationComponent),
			OwnerReferences: []metav1.OwnerReference{ownerReference(owner)},
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name:                    validationWebhookName,
				AdmissionReviewVersions: []string{"v1"},
				SideEffects:             &sideEffects,
				FailurePolicy:           &failurePolicy,
				MatchPolicy:             &matchPolicy,
				TimeoutSeconds:          &timeout,
				ClientConfig:            m.clientConfig("/validate-delete", caBundle),
				Rules:                   protectedWorkloadRules(),
				ObjectSelector:          &metav1.LabelSelector{MatchLabels: map[string]string{protectedLabel: "true"}},
			},
			{
				Name:                    validationCRWebhookName,
				AdmissionReviewVersions: []string{"v1"},
				SideEffects:             &sideEffects,
				FailurePolicy:           &failurePolicy,
				MatchPolicy:             &matchPolicy,
				TimeoutSeconds:          &timeout,
				ClientConfig:            m.clientConfig("/validate-delete", caBundle),
				Rules:                   customResourceRules(),
			},
			{
				Name:                    validationCRDWebhookName,
				AdmissionReviewVersions: []string{"v1"},
				SideEffects:             &sideEffects,
				FailurePolicy:           &failurePolicy,
				MatchPolicy:             &matchPolicy,
				TimeoutSeconds:          &timeout,
				ClientConfig:            m.clientConfig("/validate-delete", caBundle),
				Rules:                   customResourceDefinitionRules(),
				MatchConditions: []admissionv1.MatchCondition{{
					Name:       "shiftpv-crd",
					Expression: "request.name in ['shiftpvpools.shiftpv.io', 'shiftpvvolumes.shiftpv.io', 'shiftpvmoves.shiftpv.io']",
				}},
			},
		},
	}
}

// protectedWorkloadRules guards deletion of the built-in resources that carry
// the uninstall protection label.
func protectedWorkloadRules() []admissionv1.RuleWithOperations {
	return []admissionv1.RuleWithOperations{
		{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"services", "serviceaccounts"}}},
		{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments", "daemonsets"}}},
		{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{"rbac.authorization.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}}},
		{Operations: []admissionv1.OperationType{admissionv1.Delete}, Rule: admissionv1.Rule{APIGroups: []string{"storage.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"storageclasses", "csidrivers"}}},
	}
}

// customResourceRules guards deletion of ShiftPV custom resources and Pool
// spec updates.
func customResourceRules() []admissionv1.RuleWithOperations {
	return []admissionv1.RuleWithOperations{
		{
			Operations: []admissionv1.OperationType{admissionv1.Delete},
			Rule: admissionv1.Rule{
				APIGroups:   []string{"shiftpv.io"},
				APIVersions: []string{"v1alpha1"},
				Resources:   []string{"shiftpvpools", "shiftpvvolumes", "shiftpvmoves"},
			},
		},
		{
			Operations: []admissionv1.OperationType{admissionv1.Update},
			Rule: admissionv1.Rule{
				APIGroups:   []string{"shiftpv.io"},
				APIVersions: []string{"v1alpha1"},
				Resources:   []string{"shiftpvpools"},
			},
		},
	}
}

// customResourceDefinitionRules guards deletion of the ShiftPV CRDs themselves.
func customResourceDefinitionRules() []admissionv1.RuleWithOperations {
	return []admissionv1.RuleWithOperations{{
		Operations: []admissionv1.OperationType{admissionv1.Delete},
		Rule: admissionv1.Rule{
			APIGroups:   []string{"apiextensions.k8s.io"},
			APIVersions: []string{"v1"},
			Resources:   []string{"customresourcedefinitions"},
		},
	}}
}

func (m *Manager) clientConfig(path string, caBundle []byte) admissionv1.WebhookClientConfig {
	port := int32(443)
	return admissionv1.WebhookClientConfig{
		Service:  &admissionv1.ServiceReference{Namespace: m.Config.Namespace, Name: m.Config.ServiceName, Path: &path, Port: &port},
		CABundle: append([]byte(nil), caBundle...),
	}
}

func ownerReference(owner *storagev1.CSIDriver) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "storage.k8s.io/v1", Kind: "CSIDriver", Name: owner.Name, UID: owner.UID}
}

func csiDriverOwner(owner *storagev1.CSIDriver) managedOwner {
	return managedOwner{apiVersion: "storage.k8s.io/v1", kind: "CSIDriver", name: owner.Name, uid: owner.UID}
}
