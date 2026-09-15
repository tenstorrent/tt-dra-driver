{{/* SPDX-License-Identifier: Apache-2.0 */}}
{{/* SPDX-FileCopyrightText: 2026 Tenstorrent USA, Inc. */}}

{{/*
Expand the name of the chart.
*/}}
{{- define "tt-dra-driver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "tt-dra-driver.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts.
*/}}
{{- define "tt-dra-driver.namespace" -}}
  {{- if .Values.namespaceOverride -}}
    {{- .Values.namespaceOverride -}}
  {{- else -}}
    {{- .Release.Namespace -}}
  {{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "tt-dra-driver.chart" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" $name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "tt-dra-driver.labels" -}}
helm.sh/chart: {{ include "tt-dra-driver.chart" . }}
{{ include "tt-dra-driver.templateLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Template labels.
*/}}
{{- define "tt-dra-driver.templateLabels" -}}
app.kubernetes.io/name: {{ include "tt-dra-driver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Values.selectorLabelsOverride }}
{{ toYaml .Values.selectorLabelsOverride }}
{{- end }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "tt-dra-driver.selectorLabels" -}}
{{- if .Values.selectorLabelsOverride -}}
{{ toYaml .Values.selectorLabelsOverride }}
{{- else -}}
{{ include "tt-dra-driver.templateLabels" . }}
{{- end }}
{{- end }}

{{/*
Full image name with tag.
*/}}
{{- define "tt-dra-driver.fullimage" -}}
{{- .Values.image.repository -}}:{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "tt-dra-driver.serviceAccountName" -}}
{{- $name := printf "%s-service-account" (include "tt-dra-driver.fullname" .) }}
{{- if .Values.serviceAccount.create }}
{{- default $name .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Get the latest available resource.k8s.io API version.
Returns the highest available version or an empty string if none found.
*/}}
{{- define "tt-dra-driver.resourceApiVersion" -}}
{{- if .Capabilities.APIVersions.Has "resource.k8s.io/v1" -}}
resource.k8s.io/v1
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta2" -}}
resource.k8s.io/v1beta2
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta1" -}}
resource.k8s.io/v1beta1
{{- else -}}
{{- end -}}
{{- end -}}

{{/*
The DRA driver name. Defaults to a profile-aware value when not overridden.
*/}}
{{- define "tt-dra-driver.driverName" -}}
{{- if .Values.driverName -}}
{{- .Values.driverName -}}
{{- else if eq .Values.deviceProfile "tenstorrent" -}}
tenstorrent.com
{{- else -}}
{{ printf "%s.tenstorrent.com" .Values.deviceProfile }}
{{- end -}}
{{- end -}}
