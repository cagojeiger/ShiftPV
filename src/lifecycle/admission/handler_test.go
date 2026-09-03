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
	"k8s.io/apimachinery/pkg/types"

	uninstallcheck "github.com/cagojeiger/ShiftPV/src/lifecycle/uninstall"
)

type fakeChecker struct {
	report uninstallcheck.Report
	err    error
}

func (f fakeChecker) Check(context.Context) (uninstallcheck.Report, error) { return f.report, f.err }

type fakePermit struct {
	granted bool
	err     error
}

func (f fakePermit) Granted(context.Context) (bool, error) { return f.granted, f.err }

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
