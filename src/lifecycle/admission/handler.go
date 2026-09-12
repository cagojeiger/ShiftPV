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
	CheckPoolDelete(context.Context, string, types.UID) (uninstallcheck.Report, error)
}

type Permit interface {
	Granted(context.Context) (bool, error)
}

type Handler struct {
	Checker               Checker
	Permit                Permit
	TrustedRuntimeDeleter string
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
	if trustedRuntimeDelete(request, h.TrustedRuntimeDeleter) {
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
	if isPoolDelete(request) {
		poolUID, err := deletedObjectUID(request)
		if err != nil {
			return denied(fmt.Sprintf("ShiftPV Pool deletion denied: %v", err), request.UID)
		}
		report, err := h.Checker.CheckPoolDelete(ctx, request.Name, poolUID)
		if err != nil {
			return denied(fmt.Sprintf("ShiftPV Pool deletion denied: inspect Pool dependencies: %v", err), request.UID)
		}
		if report.Safe() {
			return &admissionv1.AdmissionResponse{UID: request.UID, Allowed: true}
		}
		return denied("ShiftPV Pool deletion denied: dependent storage exists: "+blockerNames(report), request.UID)
	}
	report, err := h.Checker.Check(ctx)
	if err != nil {
		return denied(fmt.Sprintf("ShiftPV resource deletion denied: inspect dependencies: %v", err), request.UID)
	}
	if report.Safe() {
		return denied("ShiftPV resource deletion denied: use the Helm or Argo CD uninstall guard to begin a quiesced teardown", request.UID)
	}

	return denied("ShiftPV resource deletion denied: dependent storage exists: "+blockerNames(report), request.UID)
}

func blockerNames(report uninstallcheck.Report) string {
	blockers := make([]string, 0, len(report.Blockers))
	for _, blocker := range report.Blockers {
		name := blocker.Name
		if blocker.Namespace != "" {
			name = blocker.Namespace + "/" + name
		}
		blockers = append(blockers, fmt.Sprintf("%s %s", blocker.Kind, name))
	}
	return strings.Join(blockers, ", ")
}

func isPoolDelete(request *admissionv1.AdmissionRequest) bool {
	return request.Resource.Group == "shiftpv.io" && request.Resource.Resource == "shiftpvpools"
}

func deletedObjectUID(request *admissionv1.AdmissionRequest) (types.UID, error) {
	if request.Name == "" {
		return "", fmt.Errorf("Pool name is missing")
	}
	var metadataOnly struct {
		Metadata struct {
			UID types.UID `json:"uid"`
		} `json:"metadata"`
	}
	if len(request.OldObject.Raw) == 0 {
		return "", fmt.Errorf("Pool identity is missing")
	}
	if err := json.Unmarshal(request.OldObject.Raw, &metadataOnly); err != nil {
		return "", fmt.Errorf("read Pool identity: %w", err)
	}
	if metadataOnly.Metadata.UID == "" {
		return "", fmt.Errorf("Pool UID is missing")
	}
	return metadataOnly.Metadata.UID, nil
}

func trustedRuntimeDelete(request *admissionv1.AdmissionRequest, trustedUsername string) bool {
	if trustedUsername == "" || request.UserInfo.Username != trustedUsername || request.Resource.Group != "shiftpv.io" {
		return false
	}
	switch request.Resource.Resource {
	case "shiftpvvolumes", "shiftpvmoves", "shiftpvcleanups":
		return true
	default:
		return false
	}
}

func denied(message string, uid types.UID) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     uid,
		Allowed: false,
		Result:  &metav1.Status{Status: metav1.StatusFailure, Message: message, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden},
	}
}
