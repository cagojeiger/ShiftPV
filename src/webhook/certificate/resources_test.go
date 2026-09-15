package certificate

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureManagedConverge(t *testing.T) {
	owner := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "shiftpv-webhook", Namespace: "shiftpv-system", UID: types.UID("service-uid")}}
	unmanagedLabels := map[string]string{NameLabel: "other", ComponentLabel: secretComponent, ManagedByLabel: ManagedByValue}

	cases := []struct {
		name      string
		existing  *corev1.ConfigMap
		wantErr   string
		wantVerbs []string
		wantData  map[string]string
	}{
		{
			name:      "absent resource is created",
			existing:  nil,
			wantVerbs: []string{"get", "create"},
			wantData:  map[string]string{"state": "desired"},
		},
		{
			name:      "unmanaged resource is refused",
			existing:  managedConfigMap(owner, unmanagedLabels, map[string]string{"state": "foreign"}),
			wantErr:   `refuse to update unmanaged test ConfigMap "shiftpv-test"`,
			wantVerbs: []string{"get"},
			wantData:  map[string]string{"state": "foreign"},
		},
		{
			name:      "unchanged resource is not updated",
			existing:  managedConfigMap(owner, ManagedLabels(secretComponent), map[string]string{"state": "desired"}),
			wantVerbs: []string{"get"},
			wantData:  map[string]string{"state": "desired"},
		},
		{
			name:      "drifted resource is updated",
			existing:  managedConfigMap(owner, ManagedLabels(secretComponent), map[string]string{"state": "stale"}),
			wantVerbs: []string{"get", "update"},
			wantData:  map[string]string{"state": "desired"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var objects []runtime.Object
			if testCase.existing != nil {
				objects = append(objects, testCase.existing)
			}
			client := fake.NewSimpleClientset(objects...)
			desired := managedConfigMap(owner, ManagedLabels(secretComponent), map[string]string{"state": "desired"})

			err := ensureTestConfigMap(context.Background(), client, owner, desired)
			switch {
			case testCase.wantErr == "" && err != nil:
				t.Fatalf("ensureManaged: %v", err)
			case testCase.wantErr != "" && (err == nil || !strings.Contains(err.Error(), testCase.wantErr)):
				t.Fatalf("ensureManaged error = %v, want %q", err, testCase.wantErr)
			}

			var verbs []string
			for _, action := range client.Actions() {
				verbs = append(verbs, action.GetVerb())
			}
			if !reflect.DeepEqual(verbs, testCase.wantVerbs) {
				t.Fatalf("actions = %v, want %v", verbs, testCase.wantVerbs)
			}

			stored, getErr := client.CoreV1().ConfigMaps(owner.Namespace).Get(context.Background(), "shiftpv-test", metav1.GetOptions{})
			if getErr != nil {
				t.Fatalf("read stored ConfigMap: %v", getErr)
			}
			if !reflect.DeepEqual(stored.Data, testCase.wantData) {
				t.Fatalf("stored data = %v, want %v", stored.Data, testCase.wantData)
			}
		})
	}
}

func TestEnsureManagedWrapsReadFailure(t *testing.T) {
	owner := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "shiftpv-webhook", Namespace: "shiftpv-system", UID: types.UID("service-uid")}}
	request := ensureRequest[*corev1.ConfigMap]{
		resource:  "test ConfigMap",
		component: secretComponent,
		owner:     managedOwner{apiVersion: "v1", kind: "Service", name: owner.Name, uid: owner.UID},
		desired:   managedConfigMap(owner, ManagedLabels(secretComponent), map[string]string{"state": "desired"}),
		get: func(context.Context) (*corev1.ConfigMap, error) {
			return nil, context.DeadlineExceeded
		},
		create: func(context.Context, *corev1.ConfigMap) error { return nil },
		update: func(context.Context, *corev1.ConfigMap) error { return nil },
		equal:  func(existing, desired *corev1.ConfigMap) bool { return true },
	}
	err := ensureManaged(context.Background(), request)
	if err == nil || err.Error() != "read test ConfigMap: context deadline exceeded" {
		t.Fatalf("ensureManaged error = %v", err)
	}
}

func ensureTestConfigMap(ctx context.Context, client kubernetes.Interface, owner *corev1.Service, desired *corev1.ConfigMap) error {
	configMaps := client.CoreV1().ConfigMaps(owner.Namespace)
	return ensureManaged(ctx, ensureRequest[*corev1.ConfigMap]{
		resource:  "test ConfigMap",
		component: secretComponent,
		owner:     managedOwner{apiVersion: "v1", kind: "Service", name: owner.Name, uid: owner.UID},
		desired:   desired,
		get: func(ctx context.Context) (*corev1.ConfigMap, error) {
			return configMaps.Get(ctx, desired.Name, metav1.GetOptions{})
		},
		create: func(ctx context.Context, desired *corev1.ConfigMap) error {
			_, err := configMaps.Create(ctx, desired, metav1.CreateOptions{})
			return err
		},
		update: func(ctx context.Context, desired *corev1.ConfigMap) error {
			_, err := configMaps.Update(ctx, desired, metav1.UpdateOptions{})
			return err
		},
		equal: func(existing, desired *corev1.ConfigMap) bool {
			return reflect.DeepEqual(existing.Data, desired.Data) &&
				reflect.DeepEqual(existing.Labels, desired.Labels) &&
				reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences)
		},
	})
}

func managedConfigMap(owner *corev1.Service, labels, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "shiftpv-test",
			Namespace:       owner.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: owner.Name, UID: owner.UID}},
		},
		Data: data,
	}
}
