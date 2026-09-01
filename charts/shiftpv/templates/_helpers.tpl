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

{{- define "shiftpv.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "shiftpv.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "shiftpv.controllerServiceAccount" -}}
{{- default (printf "%s-controller" (include "shiftpv.fullname" .)) .Values.serviceAccount.controller.name }}
{{- end }}

{{- define "shiftpv.nodeServiceAccount" -}}
{{- default (printf "%s-node" (include "shiftpv.fullname" .)) .Values.serviceAccount.node.name }}
{{- end }}
