{{/*
Expand the name of the chart.
*/}}
{{- define "adaptr.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Truncate at 63 chars because some Kubernetes name fields are limited to this.
If release name contains chart name it will be used as a full name.
*/}}
{{- define "adaptr.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart label value (name-version).
*/}}
{{- define "adaptr.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "adaptr.labels" -}}
helm.sh/chart: {{ include "adaptr.chart" . }}
{{ include "adaptr.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — used in Deployment matchLabels and Service selector.
These must be stable across upgrades; do not add values-driven labels here.
*/}}
{{- define "adaptr.selectorLabels" -}}
app.kubernetes.io/name: {{ include "adaptr.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name to use for the Deployment.
*/}}
{{- define "adaptr.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "adaptr.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Resolve the container port from values.
Uses config.externalPort as the canonical source of truth.
*/}}
{{- define "adaptr.containerPort" -}}
{{- .Values.config.externalPort | int }}
{{- end }}

{{/*
Resolve the Service targetPort.
Falls back to config.externalPort when service.targetPort is not set.
*/}}
{{- define "adaptr.serviceTargetPort" -}}
{{- if .Values.service.targetPort }}
{{- .Values.service.targetPort }}
{{- else }}
{{- .Values.config.externalPort | int }}
{{- end }}
{{- end }}
