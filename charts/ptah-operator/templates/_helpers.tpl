{{- define "ptah-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ptah-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "ptah-operator.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "ptah-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "ptah-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ptah-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "ptah-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ptah-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.certRotatorServiceAccountName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 39 | trimSuffix "-" -}}
{{- printf "%s-cert-rotator" $base -}}
{{- end -}}

{{- define "ptah-operator.certRotationLeaseName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 49 | trimSuffix "-" -}}
{{- printf "%s-cert-rotation" $base -}}
{{- end -}}

{{- define "ptah-operator.webhookSecretName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 50 | trimSuffix "-" -}}
{{- default (printf "%s-webhook-cert" $base) .Values.webhook.existingSecret -}}
{{- end -}}

{{- define "ptah-operator.webhookServiceName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 55 | trimSuffix "-" -}}
{{- printf "%s-webhook" $base -}}
{{- end -}}

{{- define "ptah-operator.metricsServiceName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 55 | trimSuffix "-" -}}
{{- printf "%s-metrics" $base -}}
{{- end -}}

{{- define "ptah-operator.approvalWebhookConfigurationName" -}}
{{- "ptah-operator-admission" -}}
{{- end -}}

{{- define "ptah-operator.webhookEntryCABundle" -}}
{{- $result := .newBundle -}}
{{- $existingBundle := default "" .existingBundle -}}
{{- if and .secretExists $existingBundle -}}
{{- $result = $existingBundle -}}
{{- else if $existingBundle -}}
{{- $decoded := $existingBundle | b64dec -}}
{{- $certificatePEM := `(?s)^[[:space:]]*(-----BEGIN CERTIFICATE-----[[:space:]]+[A-Za-z0-9+/=[:space:]]+-----END CERTIFICATE-----[[:space:]]*)+$` -}}
{{- if regexMatch $certificatePEM $decoded -}}
{{- $result = printf "%s%s" $decoded (.newBundle | b64dec) | b64enc -}}
{{- end -}}
{{- end -}}
{{- $result -}}
{{- end -}}

{{- define "ptah-operator.managerImage" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else if .Values.image.allowMutableTag -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- else -}}
{{- fail "image.digest must pin the manager with sha256:<64 lowercase hex>; image.allowMutableTag is test-only" -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.validateExecutionImages" -}}
{{- $pattern := `^[^[:space:]@]+@sha256:[0-9a-f]{64}$` -}}
{{- if not (regexMatch $pattern .Values.execution.executorImage) -}}
{{- fail "execution.executorImage must be an image pinned with @sha256:<64 lowercase hex>" -}}
{{- end -}}

{{- if not (regexMatch $pattern .Values.execution.runnerImage) -}}
{{- fail "execution.runnerImage must be an image pinned with @sha256:<64 lowercase hex>" -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.validateLeaderElection" -}}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.leaderElection) -}}
{{- fail "leaderElection must be true when replicaCount is greater than 1" -}}
{{- end -}}
{{- end -}}
