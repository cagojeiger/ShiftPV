package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	uninstallcheck "github.com/cagojeiger/ShiftPV/src/lifecycle/uninstall"
)

type fakeChecker struct {
	report     uninstallcheck.Report
	err        error
	poolReport uninstallcheck.Report
	poolErr    error
}

func (f fakeChecker) Check(context.Context) (uninstallcheck.Report, error) { return f.report, f.err }
func (f fakeChecker) CheckPoolDelete(context.Context, string, types.UID) (uninstallcheck.Report, error) {
	return f.poolReport, f.poolErr
}

type fakePermit struct {
	granted bool
	err     error
}

func (f fakePermit) Granted(context.Context) (bool, error) { return f.granted, f.err }

func TestAdmitRuntimeDeleteRequiresPermitExceptForTrustedController(t *testing.T) {
	trusted := "system:serviceaccount:shiftpv-system:shiftpv-controller"
	handler := &Handler{TrustedRuntimeDeleter: trusted}
	request := &admissionv1.AdmissionRequest{
		UID:       types.UID("runtime-delete"),
		Operation: admissionv1.Delete,
		Resource:  metav1.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvvolumes"},
		UserInfo:  authenticationv1.UserInfo{Username: trusted},
	}

	response := handler.Admit(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("trusted controller runtime deletion denied: %#v", response)
	}

	request.Resource.Resource = "shiftpvpools"
	response = handler.Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "not configured") {
		t.Fatalf("controller Pool deletion bypassed lifecycle protection: %#v", response)
	}

	request.Resource = metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	response = handler.Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "not configured") {
		t.Fatalf("controller identity bypassed chart resource protection: %#v", response)
	}

	request.Resource = metav1.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvcleanups"}
	request.UserInfo.Username = "cluster-admin"
	response = handler.Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "not configured") {
		t.Fatalf("untrusted runtime deletion bypassed lifecycle protection: %#v", response)
	}
}

func TestAdmitDeleteRequiresGrantedQuiescedTeardown(t *testing.T) {
	request := &admissionv1.AdmissionRequest{UID: types.UID("delete"), Operation: admissionv1.Delete}

	response := (&Handler{}).Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || response.Result.Code != http.StatusForbidden {
		t.Fatalf("unconfigured response = %#v", response)
	}

	response = (&Handler{Checker: fakeChecker{err: errors.New("API unavailable")}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if response.Allowed || !strings.Contains(response.Result.Message, "API unavailable") {
		t.Fatalf("API error response = %#v", response)
	}

	response = (&Handler{Checker: fakeChecker{report: uninstallcheck.Report{Blockers: []uninstallcheck.Blocker{{Kind: "PersistentVolume", Name: "pv-data"}}}}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if response.Allowed || !strings.Contains(response.Result.Message, "PersistentVolume pv-data") {
		t.Fatalf("blocked response = %#v", response)
	}

	response = (&Handler{Checker: fakeChecker{err: errors.New("must not be called")}, Permit: fakePermit{granted: true}}).Admit(context.Background(), request)
	if !response.Allowed || response.UID != request.UID {
		t.Fatalf("safe response = %#v", response)
	}

	dryRun := true
	request.DryRun = &dryRun
	response = (&Handler{Checker: fakeChecker{}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if response.Allowed || !strings.Contains(response.Result.Message, "quiesced teardown") {
		t.Fatalf("safe direct delete response = %#v", response)
	}
}

func TestAdmitAllowsOnlyDependencyFreePoolDeregistration(t *testing.T) {
	request := poolDeleteRequest("pool-a", "pool-uid")
	response := (&Handler{Checker: fakeChecker{}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if !response.Allowed {
		t.Fatalf("dependency-free Pool deletion denied: %#v", response)
	}

	blocker := uninstallcheck.Blocker{Kind: "ShiftPVPoolCopy", Name: "pool-a"}
	response = (&Handler{Checker: fakeChecker{poolReport: uninstallcheck.Report{Blockers: []uninstallcheck.Blocker{blocker}}}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "ShiftPVPoolCopy pool-a") {
		t.Fatalf("dependent Pool deletion response = %#v", response)
	}

	response = (&Handler{Checker: fakeChecker{poolErr: errors.New("inventory unavailable")}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "inventory unavailable") {
		t.Fatalf("Pool inspection failure response = %#v", response)
	}

	request.OldObject.Raw = nil
	response = (&Handler{Checker: fakeChecker{}, Permit: fakePermit{}}).Admit(context.Background(), request)
	if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "identity is missing") {
		t.Fatalf("Pool deletion without identity response = %#v", response)
	}
}

func poolDeleteRequest(name string, uid types.UID) *admissionv1.AdmissionRequest {
	oldObject, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": uid}})
	return &admissionv1.AdmissionRequest{
		UID:       types.UID("pool-delete"),
		Name:      name,
		Operation: admissionv1.Delete,
		Resource:  metav1.GroupVersionResource{Group: "shiftpv.io", Version: "v1alpha1", Resource: "shiftpvpools"},
		OldObject: runtime.RawExtension{Raw: oldObject},
	}
}

func TestAdmissionHTTPAndIgnoredOperations(t *testing.T) {
	handler := &Handler{Checker: fakeChecker{}, Permit: fakePermit{granted: true}}
	review := admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{UID: types.UID("request"), Operation: admissionv1.Delete}}
	body, _ := json.Marshal(review)
	request := httptest.NewRequest(http.MethodPost, "/validate-delete", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	var result admissionv1.AdmissionReview
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Response == nil || !result.Response.Allowed || result.Response.UID != types.UID("request") {
		t.Fatalf("response = %#v", result.Response)
	}

	badRequest := httptest.NewRequest(http.MethodPost, "/validate-delete", strings.NewReader("not-json"))
	badResponse := httptest.NewRecorder()
	handler.ServeHTTP(badResponse, badRequest)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", badResponse.Code)
	}
	if handler.Admit(context.Background(), nil).Allowed {
		t.Fatal("nil request was admitted")
	}
	if response := handler.Admit(context.Background(), &admissionv1.AdmissionRequest{Operation: admissionv1.Update}); !response.Allowed {
		t.Fatalf("non-delete response = %#v", response)
	}
}
