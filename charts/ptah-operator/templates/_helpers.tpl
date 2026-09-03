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

{{- define "ptah-operator.coordinationNamespace" -}}
{{- default .Release.Namespace .Values.coordination.namespace -}}
{{- end -}}

{{- define "ptah-operator.leaderElectionID" -}}
{{- "ptah-operator.operator.ptah.dev" -}}
{{- end -}}

{{- define "ptah-operator.crdManagerServiceAccountName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 30 | trimSuffix "-" -}}
{{- printf "%s-crd-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.teardownServiceAccountName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 30 | trimSuffix "-" -}}
{{- printf "%s-cleanup-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.teardownPrivilegeRoleName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 24 | trimSuffix "-" -}}
{{- printf "%s-cleanup-priv-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.teardownGuardRoleName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 24 | trimSuffix "-" -}}
{{- printf "%s-cleanup-guard-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.teardownDiscoveryRoleName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 20 | trimSuffix "-" -}}
{{- printf "%s-cleanup-discovery-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.teardownQuiesceJobName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 30 | trimSuffix "-" -}}
{{- printf "%s-quiesce-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.validateLifecycleResourceIdentities" -}}
{{- $releaseNamespace := .Release.Namespace -}}
{{- $coordinationNamespace := include "ptah-operator.coordinationNamespace" . -}}
{{- $controllerName := include "ptah-operator.fullname" . -}}
{{- $controllerServiceAccount := include "ptah-operator.serviceAccountName" . -}}
{{- $controllerRuntimeRole := printf "%s-runtime-admission" $controllerName -}}
{{- $certificateName := include "ptah-operator.certRotatorServiceAccountName" . -}}
{{- $certificateRuntimeEnabled := and .Values.certificateRotation.enabled (not .Values.webhook.existingSecret) -}}
{{- $hookName := include "ptah-operator.crdManagerServiceAccountName" . -}}
{{- $bootstrapName := printf "%s-bootstrap" ($hookName | trunc 53 | trimSuffix "-") -}}
{{- $probeName := printf "%s-probe" ($hookName | trunc 57 | trimSuffix "-") -}}
{{- $imageCheckName := printf "%s-image-check" ($hookName | trunc 51 | trimSuffix "-") -}}
{{- $preflightName := printf "%s-preflight" ($hookName | trunc 53 | trimSuffix "-") -}}
{{- $identityProbeName := include "ptah-operator.hookIdentityProbeJobName" . -}}
{{- $cleanupName := include "ptah-operator.teardownServiceAccountName" . -}}
{{- $quiesceName := include "ptah-operator.teardownQuiesceJobName" . -}}
{{- $cleanupPrivilegeName := include "ptah-operator.teardownPrivilegeRoleName" . -}}
{{- $cleanupGuardName := include "ptah-operator.teardownGuardRoleName" . -}}
{{- $cleanupDiscoveryName := include "ptah-operator.teardownDiscoveryRoleName" . -}}
{{- $identities := list
      (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $controllerServiceAccount "source" "controller ServiceAccount")
      (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $hookName "source" "CRD manager hook ServiceAccount")
      (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $cleanupName "source" "teardown ServiceAccount")
      (dict "kind" "ClusterRole" "namespace" "" "name" $controllerName "source" "controller ClusterRole")
      (dict "kind" "ClusterRole" "namespace" "" "name" $bootstrapName "source" "hook bootstrap ClusterRole")
      (dict "kind" "ClusterRole" "namespace" "" "name" $hookName "source" "CRD manager ClusterRole")
      (dict "kind" "ClusterRole" "namespace" "" "name" $quiesceName "source" "teardown quiesce ClusterRole")
      (dict "kind" "ClusterRole" "namespace" "" "name" $cleanupPrivilegeName "source" "teardown privilege ClusterRole")
      (dict "kind" "ClusterRole" "namespace" "" "name" $cleanupGuardName "source" "teardown residual ClusterRole")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $controllerName "source" "controller ClusterRoleBinding")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $bootstrapName "source" "hook bootstrap ClusterRoleBinding")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $hookName "source" "CRD manager ClusterRoleBinding")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $quiesceName "source" "teardown quiesce ClusterRoleBinding")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $cleanupPrivilegeName "source" "teardown privilege ClusterRoleBinding")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $cleanupGuardName "source" "teardown residual ClusterRoleBinding")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $controllerRuntimeRole "source" "controller runtime Role")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $bootstrapName "source" "hook bootstrap Role")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $hookName "source" "CRD manager Role")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $probeName "source" "hook probe Role")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $quiesceName "source" "teardown quiesce Role")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $cleanupPrivilegeName "source" "teardown privilege release Role")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $cleanupGuardName "source" "teardown residual release Role")
      (dict "kind" "Role" "namespace" $coordinationNamespace "name" $controllerName "source" "controller coordination Role")
      (dict "kind" "Role" "namespace" "default" "name" $cleanupDiscoveryName "source" "teardown discovery Role")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $controllerRuntimeRole "source" "controller runtime RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $bootstrapName "source" "hook bootstrap RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $hookName "source" "CRD manager RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $probeName "source" "hook probe RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $quiesceName "source" "teardown quiesce RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $cleanupPrivilegeName "source" "teardown privilege release RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $cleanupGuardName "source" "teardown residual release RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $coordinationNamespace "name" $controllerName "source" "controller coordination RoleBinding")
      (dict "kind" "RoleBinding" "namespace" "default" "name" $cleanupDiscoveryName "source" "teardown discovery RoleBinding")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $imageCheckName "source" "manager image-check Job")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $identityProbeName "source" "hook identity-probe Job")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $preflightName "source" "CRD preflight Job")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $hookName "source" "CRD reconcile Job")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $quiesceName "source" "teardown quiesce Job")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $cleanupName "source" "teardown cleanup Job")
-}}
{{- if $certificateRuntimeEnabled -}}
{{- $identities = append $identities (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $certificateName "source" "certificate ServiceAccount") -}}
{{- $identities = append $identities (dict "kind" "ClusterRole" "namespace" "" "name" $certificateName "source" "certificate ClusterRole") -}}
{{- $identities = append $identities (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $certificateName "source" "certificate ClusterRoleBinding") -}}
{{- $identities = append $identities (dict "kind" "Role" "namespace" $releaseNamespace "name" $certificateName "source" "certificate Role") -}}
{{- $identities = append $identities (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $certificateName "source" "certificate RoleBinding") -}}
{{- end -}}
{{- if .Values.approverClusterRole.create -}}
{{- $identities = append $identities (dict "kind" "ClusterRole" "namespace" "" "name" (printf "%s-approver" $controllerName) "source" "approver ClusterRole") -}}
{{- end -}}
{{- if ne $coordinationNamespace $releaseNamespace -}}
{{- $identities = append $identities (dict "kind" "Role" "namespace" $coordinationNamespace "name" $cleanupPrivilegeName "source" "teardown privilege coordination Role") -}}
{{- $identities = append $identities (dict "kind" "RoleBinding" "namespace" $coordinationNamespace "name" $cleanupPrivilegeName "source" "teardown privilege coordination RoleBinding") -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $identity := $identities -}}
{{- $key := printf "%s|%s|%s" $identity.kind $identity.namespace $identity.name -}}
{{- if hasKey $seen $key -}}
{{- fail (printf "lifecycle resource identity collision: %s and %s both render %s %s/%s" (index $seen $key) $identity.source $identity.kind (default "<cluster>" $identity.namespace) $identity.name) -}}
{{- end -}}
{{- $_ := set $seen $key $identity.source -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.crdManagerClusterRoleName" -}}
{{- include "ptah-operator.crdManagerServiceAccountName" . -}}
{{- end -}}

{{- define "ptah-operator.controllerStateVersion" -}}1{{- end -}}

{{- define "ptah-operator.admissionContractVersion" -}}1{{- end -}}

{{- /* Increase for every published operator release. Guard resources are
      append-only, so reusing a sequence would make a different runtime target
      an already-retained object name. */ -}}
{{- define "ptah-operator.releaseSequence" -}}1{{- end -}}

{{- define "ptah-operator.hookIdentityDigest" -}}
{{- printf "%s\n%s\n%s\n%s" .Release.Namespace .Release.Name (include "ptah-operator.releaseSequence" .) (include "ptah-operator.managerImage" .) | sha256sum -}}
{{- end -}}

{{- define "ptah-operator.hookIdentityGuardPolicyName" -}}
{{- printf "ptah-operator-hook-identity-v%s-%s" (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.hookIdentityProbeGuardPolicyName" -}}
{{- printf "ptah-operator-hook-probe-guard-v%s-%s" (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.releaseActivationGuardPolicyName" -}}
{{- printf "ptah-operator-release-activation-guard-v1-%s" (printf "%s\n%s" .Release.Namespace .Release.Name | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.namespaceDeletionGuardPolicyName" -}}
{{- printf "ptah-operator-namespace-deletion-guard-v1-%s" (printf "%s\n%s" .Release.Namespace .Release.Name | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.hookIdentityProbeJobName" -}}
{{- printf "ptah-hook-identity-v%s-%s" (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.hookIdentityProbeObjectName" -}}
{{- printf "ptah-hook-probe-v%s-%s" (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.rolloutGuardPolicyName" -}}
{{- printf "ptah-operator-rollout-guard-v%s" (include "ptah-operator.releaseSequence" .) -}}
{{- end -}}

{{- define "ptah-operator.runtimeGuardPolicyName" -}}
{{- printf "ptah-operator-runtime-guard-v%s" (include "ptah-operator.releaseSequence" .) -}}
{{- end -}}

{{- define "ptah-operator.runtimePodGuardPolicyName" -}}
{{- printf "ptah-operator-runtime-pod-identity-v%s" (include "ptah-operator.releaseSequence" .) -}}
{{- end -}}

{{- define "ptah-operator.parentReplicaSetGuardPolicyName" -}}
{{- printf "ptah-operator-runtime-parent-guard-v1-%s" (printf "%s\n%s" .Release.Namespace .Release.Name | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.parentHookPodOriginGuardPolicyName" -}}
{{- printf "ptah-operator-hook-pod-origin-guard-v1-%s" (printf "%s\n%s" .Release.Namespace .Release.Name | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.parentHookJobOriginGuardPolicyName" -}}
{{- printf "ptah-operator-hook-parent-origin-guard-v1-%s" (printf "%s\n%s" .Release.Namespace .Release.Name | sha256sum | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.parentHookJobContractPolicyName" -}}
{{- printf "ptah-operator-hook-parent-contract-v%s-%s" (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.controllerRuntimeArgsJSON" -}}
{{- $args := list
      (printf "--leader-elect=%t" .Values.leaderElection)
      (printf "--metrics-bind-address=%s" .Values.metrics.bindAddress)
      "--health-probe-bind-address=:8081"
      (printf "--webhook-port=%v" .Values.webhook.port)
      "--webhook-cert-dir=/certs"
      (printf "--controller-image=%s" (include "ptah-operator.controllerImage" .))
      (printf "--executor-image=%s" .Values.execution.executorImage)
      (printf "--runner-image=%s" .Values.execution.runnerImage)
      (printf "--ptah-version=%s" .Values.execution.ptahVersion)
      (printf "--target-lock-namespace=%s" (include "ptah-operator.coordinationNamespace" .))
      (printf "--default-tolerations-enabled=%t" .Values.admission.defaultTolerationsEnabled)
      (printf "--default-not-ready-toleration-seconds=%v" .Values.admission.defaultNotReadyTolerationSeconds)
      (printf "--default-unreachable-toleration-seconds=%v" .Values.admission.defaultUnreachableTolerationSeconds)
      (printf "--extended-resource-toleration-enabled=%t" .Values.admission.extendedResourceTolerationEnabled)
      (printf "--always-pull-images-enabled=%t" .Values.admission.alwaysPullImagesEnabled) -}}
{{- $args | toJson -}}
{{- end -}}

{{- define "ptah-operator.certificateRuntimeArgsJSON" -}}
{{- $rotatorName := include "ptah-operator.certRotatorServiceAccountName" . -}}
{{- $args := list
      (printf "--namespace=%s" .Release.Namespace)
      (printf "--secret-name=%s" (include "ptah-operator.webhookSecretName" .)) -}}
{{- if .Values.certificateRotation.recreateMissingSecret -}}
{{- $args = append $args "--recreate-missing-secret=true" -}}
{{- $args = append $args (printf "--secret-create-policy-name=%s" $rotatorName) -}}
{{- $args = append $args (printf "--secret-create-policy-binding-name=%s" $rotatorName) -}}
{{- $args = append $args (printf "--secret-create-service-account-name=%s" $rotatorName) -}}
{{- end -}}
{{- $args = concat $args (list
      (printf "--lease-name=%s" (include "ptah-operator.certRotationLeaseName" .))
      (printf "--mutating-webhook-configuration=%s" (include "ptah-operator.approvalWebhookConfigurationName" .))
      "--mutating-webhook-names=mapproval.operator.ptah.dev"
      (printf "--validating-webhook-configuration=%s" (include "ptah-operator.approvalWebhookConfigurationName" .))
      "--validating-webhook-names=vapproval.operator.ptah.dev,vpodintent.operator.ptah.dev"
      (printf "--service-name=%s" (include "ptah-operator.webhookServiceName" .))
      (printf "--service-namespace=%s" .Release.Namespace)
      "--endpoint-port-name=https"
      "--holder-identity=$(POD_NAME)/$(POD_UID)"
      (printf "--run-interval=%s" .Values.certificateRotation.interval)
      (printf "--operation-timeout=%s" .Values.certificateRotation.operationTimeout)
      (printf "--retry-initial=%s" .Values.certificateRotation.retryInitial)
      (printf "--retry-max=%s" .Values.certificateRotation.retryMax)
      (printf "--health-bind-address=:%v" .Values.certificateRotation.healthPort)
      (printf "--renewal-threshold=%s" .Values.certificateRotation.renewalThreshold)
      (printf "--serving-certificate-validity=%s" .Values.certificateRotation.servingCertificateValidity)
      (printf "--ca-certificate-validity=%s" .Values.certificateRotation.caCertificateValidity)
      (printf "--probe-timeout=%s" .Values.certificateRotation.probeTimeout)
      (printf "--probe-interval=%s" .Values.certificateRotation.probeInterval)
      (printf "--lease-duration=%s" .Values.certificateRotation.leaseDuration)
      (printf "--lease-acquire-timeout=%s" .Values.certificateRotation.leaseAcquireTimeout)) -}}
{{- $args | toJson -}}
{{- end -}}

{{- define "ptah-operator.celExactStringMapExpression" -}}
{{- $path := .path -}}
{{- $values := default (dict) .values -}}
{{- if eq (len $values) 0 -}}
{{- printf `(!has(%[1]s) || %[1]s.size() == 0)` $path -}}
{{- else -}}
{{- $parts := list (printf `has(%s)` $path) (printf `%s.size() == %d` $path (len $values)) -}}
{{- range $key := keys $values | sortAlpha -}}
{{- $parts = append $parts (printf `%q in %[2]s && %[2]s[%[1]q] == %[3]q` $key $path (index $values $key)) -}}
{{- end -}}
{{- join " && " $parts -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.celExactOpaqueExpression" -}}
{{- $path := .path -}}
{{- $value := .value -}}
{{- if empty $value -}}
{{- if eq .kind "object" -}}
{{- printf `!has(%s)` $path -}}
{{- else -}}
{{- printf `(!has(%[1]s) || %[1]s.size() == 0)` $path -}}
{{- end -}}
{{- else -}}
{{- printf `has(%s) && dyn(%s) == %s` $path $path ($value | toJson) -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.celExactResourcesExpression" -}}
{{- $container := .container -}}
{{- $resources := default (dict) .resources -}}
{{- $resourcesPath := printf "dyn(%s.resources)" $container -}}
{{- $parts := list (printf `has(%s.resources)` $container) -}}
{{- range $bucket := list "limits" "requests" -}}
{{- $values := default (dict) (index $resources $bucket) -}}
{{- $path := printf "%s.%s" $resourcesPath $bucket -}}
{{- if eq (len $values) 0 -}}
{{- $parts = append $parts (printf `(!has(%[1]s) || %[1]s.size() == 0)` $path) -}}
{{- else -}}
{{- $parts = append $parts (printf `has(%s) && %s.size() == %d` $path $path (len $values)) -}}
{{- range $key := keys $values | sortAlpha -}}
{{- $parts = append $parts (printf `%q in %[2]s && quantity(string(%[2]s[%[1]q])).compareTo(quantity(%[3]q)) == 0` $key $path (printf "%v" (index $values $key))) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $parts = append $parts (printf `(!has(%[1]s.claims) || %[1]s.claims.size() == 0)` $resourcesPath) -}}
{{- join " && " $parts -}}
{{- end -}}

{{- /* Pod API defaulting copies every limit into a missing same-key request.
      Workload templates are not defaulted this way, so their retained
      Deployment contract deliberately continues to use the raw values. */ -}}
{{- define "ptah-operator.celExactPodResourcesExpression" -}}
{{- $resources := deepCopy (default (dict) .resources) -}}
{{- $limits := deepCopy (default (dict) (index $resources "limits")) -}}
{{- $requests := deepCopy (default (dict) (index $resources "requests")) -}}
{{- range $key, $value := $limits -}}
{{- if not (hasKey $requests $key) -}}
{{- $_ := set $requests $key $value -}}
{{- end -}}
{{- end -}}
{{- $_ := set $resources "limits" $limits -}}
{{- $_ := set $resources "requests" $requests -}}
{{- include "ptah-operator.celExactResourcesExpression" (dict "container" .container "resources" $resources) -}}
{{- end -}}

{{- define "ptah-operator.celExactTolerationsExpression" -}}
{{- $path := .path -}}
{{- $values := default (list) .values -}}
{{- $parts := list -}}
{{- $suppressNotReady := false -}}
{{- $suppressUnreachable := false -}}
{{- range $index, $value := $values -}}
{{- $key := default "" (index $value "key") -}}
{{- $operator := default "Equal" (index $value "operator") -}}
{{- $tolerationValue := default "" (index $value "value") -}}
{{- $effect := default "" (index $value "effect") -}}
{{- $entry := printf `%[1]s[%[2]d].key == %[3]q && (%[1]s[%[2]d].operator == "" ? "Equal" : %[1]s[%[2]d].operator) == %[4]q && %[1]s[%[2]d].value == %[5]q && %[1]s[%[2]d].effect == %[6]q` $path $index $key $operator $tolerationValue $effect -}}
{{- if hasKey $value "tolerationSeconds" -}}
{{- $entry = printf `%s && has(%s[%d].tolerationSeconds) && %s[%d].tolerationSeconds == %d` $entry $path $index $path $index (int64 (index $value "tolerationSeconds")) -}}
{{- else -}}
{{- $entry = printf `%s && !has(%s[%d].tolerationSeconds)` $entry $path $index -}}
{{- end -}}
{{- $parts = append $parts (printf `(%s)` $entry) -}}
{{- if and (or (eq $key "") (eq $key "node.kubernetes.io/not-ready")) (or (eq $effect "") (eq $effect "NoExecute")) -}}
{{- $suppressNotReady = true -}}
{{- end -}}
{{- if and (or (eq $key "") (eq $key "node.kubernetes.io/unreachable")) (or (eq $effect "") (eq $effect "NoExecute")) -}}
{{- $suppressUnreachable = true -}}
{{- end -}}
{{- end -}}
{{- $expected := len $values -}}
{{- if .includeDefaults -}}
{{- if and .defaultTolerationsEnabled (not $suppressNotReady) -}}
{{- $entry := printf `%[1]s[%[2]d].key == "node.kubernetes.io/not-ready" && %[1]s[%[2]d].operator == "Exists" && %[1]s[%[2]d].value == "" && %[1]s[%[2]d].effect == "NoExecute" && has(%[1]s[%[2]d].tolerationSeconds) && %[1]s[%[2]d].tolerationSeconds == %[3]d` $path $expected (int64 .defaultNotReadyTolerationSeconds) -}}
{{- $parts = append $parts (printf `(%s)` $entry) -}}
{{- $expected = add1 $expected -}}
{{- end -}}
{{- if and .defaultTolerationsEnabled (not $suppressUnreachable) -}}
{{- $entry := printf `%[1]s[%[2]d].key == "node.kubernetes.io/unreachable" && %[1]s[%[2]d].operator == "Exists" && %[1]s[%[2]d].value == "" && %[1]s[%[2]d].effect == "NoExecute" && has(%[1]s[%[2]d].tolerationSeconds) && %[1]s[%[2]d].tolerationSeconds == %[3]d` $path $expected (int64 .defaultUnreachableTolerationSeconds) -}}
{{- $parts = append $parts (printf `(%s)` $entry) -}}
{{- $expected = add1 $expected -}}
{{- end -}}
{{- end -}}
{{- if eq $expected 0 -}}
{{- printf `(!has(%[1]s) || %[1]s.size() == 0)` $path -}}
{{- else -}}
{{- printf `has(%s) && %s.size() == %d && %s` $path $path $expected (join " && " $parts) -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.runtimeDeploymentConfigExpressionsJSON" -}}
{{- $root := . -}}
{{- $controllerDeployment := include "ptah-operator.fullname" $root -}}
{{- $isController := printf `request.name == %q` $controllerDeployment -}}
{{- $template := "object.spec.template" -}}
{{- $pod := printf "%s.spec" $template -}}
{{- $init := printf "%s.initContainers[0]" $pod -}}
{{- $app := printf "%s.containers[0]" $pod -}}
{{- $selectorLabels := include "ptah-operator.selectorLabels" $root | fromYaml -}}
{{- $controllerLabels := deepCopy $selectorLabels -}}
{{- $_ := set $controllerLabels "app.kubernetes.io/component" "controller" -}}
{{- range $key, $value := default (dict) $root.Values.podLabels -}}
{{- $_ = set $controllerLabels $key $value -}}
{{- end -}}
{{- $certificateLabels := include "ptah-operator.labels" $root | fromYaml -}}
{{- $_ = set $certificateLabels "app.kubernetes.io/component" "certificate-rotation" -}}
{{- $controllerAnnotations := dict
      "operator.ptah.dev/controller-state-version" (include "ptah-operator.controllerStateVersion" $root)
      "operator.ptah.dev/release-sequence" (include "ptah-operator.releaseSequence" $root) -}}
{{- range $key, $value := default (dict) $root.Values.podAnnotations -}}
{{- $_ = set $controllerAnnotations $key $value -}}
{{- end -}}
{{- $certificateAnnotations := dict
      "operator.ptah.dev/controller-state-version" (include "ptah-operator.controllerStateVersion" $root)
      "operator.ptah.dev/release-sequence" (include "ptah-operator.releaseSequence" $root) -}}
{{- $initResources := dict "requests" (dict "cpu" "5m" "memory" "16Mi") "limits" (dict "memory" "32Mi") -}}
{{- $controllerSelector := deepCopy $selectorLabels -}}
{{- $_ = set $controllerSelector "app.kubernetes.io/component" "controller" -}}
{{- $certificateSelector := deepCopy $selectorLabels -}}
{{- $_ = set $certificateSelector "app.kubernetes.io/component" "certificate-rotation" -}}
{{- $selectorExpression := printf `%s ? (%s) : (%s)` $isController
      (include "ptah-operator.celExactStringMapExpression" (dict "path" "object.spec.selector.matchLabels" "values" $controllerSelector))
      (include "ptah-operator.celExactStringMapExpression" (dict "path" "object.spec.selector.matchLabels" "values" $certificateSelector)) -}}
{{- $priorityExpression := printf `(!has(%[1]s.priorityClassName) || %[1]s.priorityClassName == "")` $pod -}}
{{- if ne $root.Values.priorityClassName "" -}}
{{- $priorityExpression = printf `has(%[1]s.priorityClassName) && %[1]s.priorityClassName == %[2]q` $pod $root.Values.priorityClassName -}}
{{- end -}}
{{- $expressions := list
      (printf `object.spec.replicas == (%s ? %d : 1)` $isController (int $root.Values.replicaCount))
      (printf `object.spec.strategy.type == "Recreate" && !has(object.spec.strategy.rollingUpdate) && object.spec.minReadySeconds == 0 && (!has(object.spec.paused) || !object.spec.paused) && has(object.spec.revisionHistoryLimit) && object.spec.revisionHistoryLimit == (%s ? 10 : 2) && has(object.spec.progressDeadlineSeconds) && object.spec.progressDeadlineSeconds == 600` $isController)
      (printf `has(object.spec.selector) && (%s) && (!has(object.spec.selector.matchExpressions) || object.spec.selector.matchExpressions.size() == 0)` $selectorExpression)
      (printf `%s ? (%s) : (%s)` $isController (include "ptah-operator.celExactStringMapExpression" (dict "path" (printf "%s.metadata.labels" $template) "values" $controllerLabels)) (include "ptah-operator.celExactStringMapExpression" (dict "path" (printf "%s.metadata.labels" $template) "values" $certificateLabels)))
      (printf `%s ? (%s) : (%s)` $isController (include "ptah-operator.celExactStringMapExpression" (dict "path" (printf "%s.metadata.annotations" $template) "values" $controllerAnnotations)) (include "ptah-operator.celExactStringMapExpression" (dict "path" (printf "%s.metadata.annotations" $template) "values" $certificateAnnotations)))
      (printf `%[1]s.restartPolicy == "Always" && %[1]s.dnsPolicy == "ClusterFirst" && %[1]s.schedulerName == "default-scheduler" && has(%[1]s.terminationGracePeriodSeconds) && %[1]s.terminationGracePeriodSeconds == 30 && (!has(%[1]s.nodeName) || %[1]s.nodeName == "") && !has(%[1]s.hostname) && !has(%[1]s.subdomain) && !has(%[1]s.dnsConfig) && (!has(%[1]s.hostAliases) || %[1]s.hostAliases.size() == 0) && (!has(%[1]s.readinessGates) || %[1]s.readinessGates.size() == 0) && (!has(%[1]s.schedulingGates) || %[1]s.schedulingGates.size() == 0) && !has(%[1]s.runtimeClassName) && !has(dyn(%[1]s).overhead) && !has(%[1]s.os) && (!has(%[1]s.setHostnameAsFQDN) || !%[1]s.setHostnameAsFQDN)` $pod)
      (include "ptah-operator.celExactOpaqueExpression" (dict "path" (printf "%s.imagePullSecrets" $pod) "value" $root.Values.imagePullSecrets "kind" "list"))
      (include "ptah-operator.celExactStringMapExpression" (dict "path" (printf "%s.nodeSelector" $pod) "values" $root.Values.nodeSelector))
      (include "ptah-operator.celExactOpaqueExpression" (dict "path" (printf "%s.affinity" $pod) "value" $root.Values.affinity "kind" "object"))
      (include "ptah-operator.celExactTolerationsExpression" (dict "path" (printf "%s.tolerations" $pod) "values" $root.Values.tolerations "includeDefaults" false))
      $priorityExpression
      (printf `%[1]s.imagePullPolicy == %[3]q && %[2]s.imagePullPolicy == %[3]q` $init $app $root.Values.image.pullPolicy)
      (include "ptah-operator.celExactResourcesExpression" (dict "container" $init "resources" $initResources))
      (printf `%s ? (%s) : (%s)` $isController (include "ptah-operator.celExactResourcesExpression" (dict "container" $app "resources" $root.Values.resources)) (include "ptah-operator.celExactResourcesExpression" (dict "container" $app "resources" $root.Values.certificateRotation.resources)) ) -}}
{{- $expressions | toJson -}}
{{- end -}}

{{- define "ptah-operator.runtimePodConfigExpressionsJSON" -}}
{{- $root := . -}}
{{- $pod := "object.spec" -}}
{{- $init := "object.spec.initContainers[0]" -}}
{{- $app := "object.spec.containers[0]" -}}
{{- $initResources := dict "requests" (dict "cpu" "5m" "memory" "16Mi") "limits" (dict "memory" "32Mi") -}}
{{- $podPullPolicy := $root.Values.image.pullPolicy -}}
{{- if $root.Values.admission.alwaysPullImagesEnabled -}}
{{- $podPullPolicy = "Always" -}}
{{- end -}}
{{- $priorityExpression := printf `(!has(%[1]s.priorityClassName) || %[1]s.priorityClassName == "") && has(%[1]s.priority) && %[1]s.priority == 0 && has(%[1]s.preemptionPolicy) && %[1]s.preemptionPolicy == "PreemptLowerPriority"` $pod -}}
{{- if ne $root.Values.priorityClassName "" -}}
{{- $priorityExpression = printf `has(%[1]s.priorityClassName) && %[1]s.priorityClassName == %[2]q && has(%[1]s.priority) && %[1]s.priority == %[3]d && has(%[1]s.preemptionPolicy) && %[1]s.preemptionPolicy == %[4]q` $pod $root.Values.priorityClassName (int $root.Values.priorityClassValue) $root.Values.priorityClassPreemptionPolicy -}}
{{- end -}}
{{- $expressions := list
      (include "ptah-operator.celExactOpaqueExpression" (dict "path" (printf "%s.imagePullSecrets" $pod) "value" $root.Values.imagePullSecrets "kind" "list"))
      (include "ptah-operator.celExactStringMapExpression" (dict "path" (printf "%s.nodeSelector" $pod) "values" $root.Values.nodeSelector))
      (include "ptah-operator.celExactOpaqueExpression" (dict "path" (printf "%s.affinity" $pod) "value" $root.Values.affinity "kind" "object"))
      (include "ptah-operator.celExactTolerationsExpression" (dict
        "path" (printf "%s.tolerations" $pod)
        "values" $root.Values.tolerations
        "includeDefaults" true
        "defaultTolerationsEnabled" $root.Values.admission.defaultTolerationsEnabled
        "defaultNotReadyTolerationSeconds" $root.Values.admission.defaultNotReadyTolerationSeconds
        "defaultUnreachableTolerationSeconds" $root.Values.admission.defaultUnreachableTolerationSeconds))
      $priorityExpression
      (printf `%[1]s.imagePullPolicy == %[3]q && %[2]s.imagePullPolicy == %[3]q` $init $app $podPullPolicy)
      (include "ptah-operator.celExactPodResourcesExpression" (dict "container" $init "resources" $initResources))
      (printf `variables.isController ? (%s) : (%s)` (include "ptah-operator.celExactPodResourcesExpression" (dict "container" $app "resources" $root.Values.resources)) (include "ptah-operator.celExactPodResourcesExpression" (dict "container" $app "resources" $root.Values.certificateRotation.resources))) -}}
{{- $expressions | toJson -}}
{{- end -}}

{{- define "ptah-operator.runtimeAdmissionContractJSON" -}}
{{- $certificateRuntimeEnabled := and .Values.certificateRotation.enabled (not .Values.webhook.existingSecret) -}}
{{- $serviceAccountAnnotations := default (dict) .Values.serviceAccount.annotations -}}
{{- $enforceMountableSecrets := default "" (index $serviceAccountAnnotations "kubernetes.io/enforce-mountable-secrets") -}}
{{- dict
      "version" 1
      "namespace" .Release.Namespace
      "commonInitContainerResources" (dict "requests" (dict "cpu" "5m" "memory" "16Mi") "limits" (dict "memory" "32Mi"))
      "controllerContainerResources" .Values.resources
      "certificateContainerResources" .Values.certificateRotation.resources
      "imagePullSecrets" (default (list) .Values.imagePullSecrets)
      "priorityClassName" .Values.priorityClassName
      "priorityClassValue" (int .Values.priorityClassValue)
      "priorityClassPreemptionPolicy" .Values.priorityClassPreemptionPolicy
      "controllerServiceAccountName" (include "ptah-operator.serviceAccountName" .)
      "certificateServiceAccountName" (include "ptah-operator.certRotatorServiceAccountName" .)
      "controllerServiceAccountCreate" .Values.serviceAccount.create
      "controllerServiceAccountEnforceMountableSecrets" (has $enforceMountableSecrets (list "1" "t" "T" "TRUE" "true" "True"))
      "controllerSecretNames" (list (include "ptah-operator.webhookSecretName" .))
      "certificateSecretNames" (list)
      "certificateRuntimeEnabled" $certificateRuntimeEnabled
    | toJson -}}
{{- end -}}

{{- define "ptah-operator.crdManagerArgsJSON" -}}
{{- $root := .root -}}
{{- $args := list
      .mode
      (printf "--timeout=%s" .timeout)
      (printf "--release-name=%s" $root.Release.Name)
      (printf "--release-namespace=%s" $root.Release.Namespace)
      (printf "--coordination-namespace=%s" (include "ptah-operator.coordinationNamespace" $root))
      (printf "--leader-election=%t" $root.Values.leaderElection)
      (printf "--leader-election-id=%s" (include "ptah-operator.leaderElectionID" $root))
      (printf "--webhook-service-name=%s" (include "ptah-operator.webhookServiceName" $root))
      (printf "--webhook-timeout-seconds=%v" $root.Values.webhook.timeoutSeconds)
      (printf "--webhook-secret-name=%s" (include "ptah-operator.webhookSecretName" $root))
      (printf "--webhook-port=%v" $root.Values.webhook.port)
      (printf "--certificate-health-port=%v" $root.Values.certificateRotation.healthPort)
      (printf "--hook-service-account-name=%s" (include "ptah-operator.crdManagerServiceAccountName" $root))
      (printf "--controller-service-account-name=%s" (include "ptah-operator.serviceAccountName" $root))
      (printf "--controller-deployment-name=%s" (include "ptah-operator.fullname" $root))
      (printf "--controller-replicas=%v" $root.Values.replicaCount)
      (printf "--certificate-deployment-name=%s" (include "ptah-operator.certRotatorServiceAccountName" $root))
      (printf "--release-sequence=%s" (include "ptah-operator.releaseSequence" $root))
      (printf "--manager-image=%s" (include "ptah-operator.managerImage" $root))
      (printf "--controller-runtime-args-b64=%s" (include "ptah-operator.controllerRuntimeArgsJSON" $root | b64enc))
      (printf "--certificate-runtime-args-b64=%s" (include "ptah-operator.certificateRuntimeArgsJSON" $root | b64enc))
      (printf "--runtime-deployment-config-expressions-b64=%s" (include "ptah-operator.runtimeDeploymentConfigExpressionsJSON" $root | b64enc))
      (printf "--runtime-pod-config-expressions-b64=%s" (include "ptah-operator.runtimePodConfigExpressionsJSON" $root | b64enc))
      (printf "--runtime-admission-contract-b64=%s" (include "ptah-operator.runtimeAdmissionContractJSON" $root | b64enc)) -}}
{{- if .verifyControllerState -}}
{{- $args = append $args "--verify-controller-state=true" -}}
{{- end -}}
{{- $args | toJson -}}
{{- end -}}

{{- define "ptah-operator.validateAdmissionSingletonObject" -}}
{{- if .object -}}
{{- $annotations := default (dict) .object.metadata.annotations -}}
{{- $labels := default (dict) .object.metadata.labels -}}
{{- if or
      (ne (default "" (index $annotations "meta.helm.sh/release-name")) .releaseName)
      (ne (default "" (index $annotations "meta.helm.sh/release-namespace")) .releaseNamespace)
      (ne (default "" (index $labels "app.kubernetes.io/managed-by")) "Helm")
      (ne (default "" (index $labels "app.kubernetes.io/instance")) .releaseName) -}}
{{- fail (printf "fixed admission singleton %s/%s is not owned by Helm release %s/%s" .kind .object.metadata.name .releaseNamespace .releaseName) -}}
{{- end -}}
{{- $present := 0 -}}
{{- $expected := merge (dict) .expectedImmutable .expectedVersions (dict "operator.ptah.dev/hook-service-account-name" .expectedHook) -}}
{{- range $key := keys $expected -}}
{{- if hasKey $annotations $key -}}
{{- $present = add1 $present -}}
{{- end -}}
{{- end -}}
{{- if and (ne $present 0) (ne $present (len $expected)) -}}
{{- fail (printf "fixed admission singleton %s/%s has an incomplete owned annotation tuple" .kind .object.metadata.name) -}}
{{- end -}}
{{- if ne $present 0 -}}
{{- range $key, $expectedValue := .expectedImmutable -}}
{{- $actual := index $annotations $key -}}
{{- if ne $actual $expectedValue -}}
{{- fail (printf "fixed admission singleton %s/%s annotation %s is %q, expected %q" $.kind $.object.metadata.name $key $actual $expectedValue) -}}
{{- end -}}
{{- end -}}
{{- range $key, $expectedValue := .expectedVersions -}}
{{- $actual := index $annotations $key -}}
{{- if not (regexMatch `^[1-9][0-9]*$` $actual) -}}
{{- fail (printf "fixed admission singleton %s/%s annotation %s is not a positive exact decimal version" $.kind $.object.metadata.name $key) -}}
{{- end -}}
{{- if gt (atoi $actual) (atoi $expectedValue) -}}
{{- fail (printf "fixed admission singleton %s/%s annotation %s is newer than candidate %s" $.kind $.object.metadata.name $key $expectedValue) -}}
{{- end -}}
{{- end -}}
{{- $actualRelease := index $annotations "operator.ptah.dev/release-sequence" -}}
{{- $actualHook := index $annotations "operator.ptah.dev/hook-service-account-name" -}}
{{- if eq $actualRelease (index .expectedVersions "operator.ptah.dev/release-sequence") -}}
{{- if ne $actualHook .expectedHook -}}
{{- fail (printf "fixed admission singleton %s/%s annotation operator.ptah.dev/hook-service-account-name is %q, expected %q" .kind .object.metadata.name $actualHook .expectedHook) -}}
{{- end -}}
{{- else -}}
{{- $currentSuffix := printf `-crd-v%s-[0-9a-f]{12}$` (index .expectedVersions "operator.ptah.dev/release-sequence") -}}
{{- $prefix := regexReplaceAll $currentSuffix .expectedHook "-crd-v" -}}
{{- $historicalPattern := printf `^%s%s-[0-9a-f]{12}$` $prefix $actualRelease -}}
{{- if not (regexMatch $historicalPattern $actualHook) -}}
{{- fail (printf "fixed admission singleton %s/%s has invalid historical hook ServiceAccount identity %q" .kind .object.metadata.name $actualHook) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* Zero owned annotations is the sole legacy state. The pre-upgrade hook
      verifies the complete predecessor webhook contract before stamping it. */ -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.validateAdmissionSingleton" -}}
{{- $name := include "ptah-operator.approvalWebhookConfigurationName" . -}}
{{- $mutating := lookup "admissionregistration.k8s.io/v1" "MutatingWebhookConfiguration" "" $name -}}
{{- $validating := lookup "admissionregistration.k8s.io/v1" "ValidatingWebhookConfiguration" "" $name -}}
{{- if or (and $mutating (not $validating)) (and $validating (not $mutating)) -}}
{{- fail (printf "fixed admission singleton %s is incomplete; both mutating and validating configurations must exist or both must be absent" $name) -}}
{{- end -}}
{{- $expectedImmutable := dict
      "operator.ptah.dev/release-name" .Release.Name
      "operator.ptah.dev/release-namespace" .Release.Namespace
      "operator.ptah.dev/coordination-namespace" (include "ptah-operator.coordinationNamespace" .)
      "operator.ptah.dev/leader-election" (printf "%t" .Values.leaderElection)
      "operator.ptah.dev/leader-election-id" (include "ptah-operator.leaderElectionID" .)
      "operator.ptah.dev/webhook-service-name" (include "ptah-operator.webhookServiceName" .)
      "operator.ptah.dev/controller-service-account-name" (include "ptah-operator.serviceAccountName" .)
      "operator.ptah.dev/controller-deployment-name" (include "ptah-operator.fullname" .)
      "operator.ptah.dev/certificate-deployment-name" (include "ptah-operator.certRotatorServiceAccountName" .) -}}
{{- $expectedVersions := dict
      "operator.ptah.dev/controller-state-version" (include "ptah-operator.controllerStateVersion" .)
      "operator.ptah.dev/admission-contract-version" (include "ptah-operator.admissionContractVersion" .)
      "operator.ptah.dev/release-sequence" (include "ptah-operator.releaseSequence" .) -}}
{{- $context := dict "expectedImmutable" $expectedImmutable "expectedVersions" $expectedVersions "expectedHook" (include "ptah-operator.crdManagerServiceAccountName" .) "releaseName" .Release.Name "releaseNamespace" .Release.Namespace -}}
{{- include "ptah-operator.validateAdmissionSingletonObject" (merge (dict "kind" "MutatingWebhookConfiguration" "object" $mutating) $context) -}}
{{- include "ptah-operator.validateAdmissionSingletonObject" (merge (dict "kind" "ValidatingWebhookConfiguration" "object" $validating) $context) -}}
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
{{- if .Values.image.testIdentityDigest -}}
{{- fail "image.testIdentityDigest must be empty when image.digest pins the production manager" -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else if .Values.image.allowMutableTag -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- else -}}
{{- fail "image.digest must pin the manager with sha256:<64 lowercase hex>; image.allowMutableTag is test-only" -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.controllerImage" -}}
{{- $pattern := `^sha256:[0-9a-f]{64}$` -}}
{{- if .Values.image.digest -}}
{{- include "ptah-operator.managerImage" . -}}
{{- else if .Values.image.allowMutableTag -}}
{{- if not (regexMatch $pattern .Values.image.testIdentityDigest) -}}
{{- fail "image.testIdentityDigest must be the exact sha256 Docker image ID when image.allowMutableTag=true" -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.testIdentityDigest -}}
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

{{- define "ptah-operator.validatePtahVersion" -}}
{{- $version := default "" .Values.execution.ptahVersion -}}
{{- if or (eq (trim $version) "") (ne (trim $version) $version) -}}
{{- fail "execution.ptahVersion is required and must identify the build in execution.executorImage" -}}
{{- end -}}
{{- if gt (len $version) 128 -}}
{{- fail "execution.ptahVersion must be at most 128 bytes" -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.validateLeaderElection" -}}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.leaderElection) -}}
{{- fail "leaderElection must be true when replicaCount is greater than 1" -}}
{{- end -}}
{{- end -}}
