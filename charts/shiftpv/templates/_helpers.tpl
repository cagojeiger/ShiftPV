{{- define "shiftpv.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "shiftpv.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- if contains (include "shiftpv.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "shiftpv.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "shiftpv.baseLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "shiftpv.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "shiftpv.labels" -}}
{{ include "shiftpv.baseLabels" . }}
shiftpv.io/uninstall-protected: "true"
{{- end }}

{{- define "shiftpv.controllerServiceAccount" -}}
{{- default (printf "%s-controller" (include "shiftpv.fullname" .)) .Values.serviceAccount.controller.name }}
{{- end }}

{{- define "shiftpv.nodeServiceAccount" -}}
{{- default (printf "%s-node" (include "shiftpv.fullname" .)) .Values.serviceAccount.node.name }}
{{- end }}

{{- define "shiftpv.uninstallGuardName" -}}
{{- printf "%s-uninstall-guard" (include "shiftpv.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "shiftpv.uninstallPermitName" -}}
{{- printf "%s-uninstall-permit" (include "shiftpv.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "shiftpv.webhookServiceName" -}}
{{- printf "%s-webhook" (include "shiftpv.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "shiftpv.webhookSecretName" -}}
{{- printf "%s-webhook-tls" (include "shiftpv.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "shiftpv.webhookConfigurationName" -}}
{{- printf "%s-mobility" (include "shiftpv.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "shiftpv.validationWebhookConfigurationName" -}}
{{- printf "%s-lifecycle" (include "shiftpv.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
