{{/*
Expand the name of the chart.
*/}}
{{- define "liqo-dynamic-offloader.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "liqo-dynamic-offloader.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "liqo-dynamic-offloader.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "liqo-dynamic-offloader.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{ include "liqo-dynamic-offloader.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "liqo-dynamic-offloader.selectorLabels" -}}
app.kubernetes.io/name: {{ include "liqo-dynamic-offloader.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
