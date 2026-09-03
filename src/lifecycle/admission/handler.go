package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	uninstallcheck "github.com/cagojeiger/ShiftPV/src/lifecycle/uninstall"
)

type Checker interface {
	Check(context.Context) (uninstallcheck.Report, error)
}

type Permit interface {
	Granted(context.Context) (bool, error)
}

type Handler struct {
	Checker Checker
	Permit  Permit
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var review admissionv1.AdmissionReview
	if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
		http.Error(writer, fmt.Sprintf("decode AdmissionReview: %v", err), http.StatusBadRequest)
		return
	}
	responseReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Response: h.Admit(request.Context(), review.Request),
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(responseReview); err != nil {
		http.Error(writer, fmt.Sprintf("encode AdmissionReview: %v", err), http.StatusInternalServerError)
	}
}

func (h *Handler) Admit(ctx context.Context, request *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if request == nil {
		return denied("AdmissionReview has no request", "")
	}
	if request.Operation != admissionv1.Delete {
		return &admissionv1.AdmissionResponse{UID: request.UID, Allowed: true}
	}
	if h.Checker == nil || h.Permit == nil {
		return denied("ShiftPV uninstall protection is not configured", request.UID)
	}
	granted, err := h.Permit.Granted(ctx)
	if err != nil {
		return denied(fmt.Sprintf("ShiftPV resource deletion denied: read uninstall permit: %v", err), request.UID)
	}
	if granted {
		return &admissionv1.AdmissionResponse{UID: request.UID, Allowed: true}
	}
	report, err := h.Checker.Check(ctx)
	if err != nil {
		return denied(fmt.Sprintf("ShiftPV resource deletion denied: inspect dependencies: %v", err), request.UID)
	}
	if report.Safe() {
		return denied("ShiftPV resource deletion denied: use the Helm or Argo CD uninstall guard to begin a quiesced teardown", request.UID)
	}

	blockers := make([]string, 0, len(report.Blockers))
	for _, blocker := range report.Blockers {
		name := blocker.Name
		if blocker.Namespace != "" {
			name = blocker.Namespace + "/" + name
		}
		blockers = append(blockers, fmt.Sprintf("%s %s", blocker.Kind, name))
	}
	return denied("ShiftPV resource deletion denied: dependent storage exists: "+strings.Join(blockers, ", "), request.UID)
}

func denied(message string, uid types.UID) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     uid,
		Allowed: false,
		Result:  &metav1.Status{Status: metav1.StatusFailure, Message: message, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden},
	}
}
