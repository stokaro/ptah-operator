{{/*
ptah-operator.migrationJobShape derives the migration Job's sealed contract from
the schema Job's by the exact substitutions that distinguish the two subjects.
internal/crdupgrade/controller_object_guard.go performs the same substitutions in
the same order, and a render test compares the two results byte for byte.
*/}}
{{- define "ptah-operator.migrationJobShape" -}}
{{- . | replace `"operator.ptah.run/schema"` `"operator.ptah.run/migration"` | replace `"schema-operation"` `"migration-operation"` | replace `["resolve", "verify", "observe", "plan", "apply"]` `["resolve", "verify", "history", "apply"]` | replace `["observe", "plan"]` `["history", "apply"]` | replace `"ptah-" + object.metadata.labels` `"ptah-m-" + object.metadata.labels` | replace `^ptah-(resolve|verify|observe|plan|apply)-` `^ptah-m-(resolve|verify|history|apply)-` | replace `"PtahSchema"` `"PtahMigration"` | replace `"fetch-schema"` `"fetch-migrations"` -}}
{{- end -}}

{{/*
ptah-operator.eitherSubject admits a Job that satisfies the schema shape or the
migration shape, and nothing else.
*/}}
{{- define "ptah-operator.eitherSubject" -}}
({{ . }}) || ({{ include "ptah-operator.migrationJobShape" . }})
{{- end -}}

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

{{- /* The controller runs as one ServiceAccount in every release, so its
      username is stable across upgrades. */ -}}
{{- define "ptah-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ptah-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Releases advance one sequence at a time, so an upgrade finds the admission
singleton written by this sequence or by the one before it.
*/}}
{{- define "ptah-operator.predecessorReleaseSequence" -}}
{{- sub (atoi (include "ptah-operator.releaseSequence" .)) 1 -}}
{{- end -}}

{{- define "ptah-operator.certRotatorServiceAccountName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 39 | trimSuffix "-" -}}
{{- printf "%s-cert-rotator" $base -}}
{{- end -}}

{{- define "ptah-operator.certRotationLeaseName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 49 | trimSuffix "-" -}}
{{- printf "%s-cert-rotation" $base -}}
{{- end -}}

{{- define "ptah-operator.certRotationStagingSecretName" -}}
{{- $base := include "ptah-operator.fullname" . | trunc 43 | trimSuffix "-" -}}
{{- printf "%s-cert-rotation-stage" $base -}}
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
{{- "ptah-operator.operator.ptah.run" -}}
{{- end -}}

{{- define "ptah-operator.crdManagerServiceAccountName" -}}
{{- /* Keep every generated Pod prefix intact through the largest positive int32 release sequence. */ -}}
{{- $base := include "ptah-operator.fullname" . | trunc 24 | trimSuffix "-" -}}
{{- printf "%s-crd-v%s-%s" $base (include "ptah-operator.releaseSequence" .) (include "ptah-operator.hookIdentityDigest" . | trunc 12) -}}
{{- end -}}

{{- define "ptah-operator.validateLifecycleResourceIdentities" -}}
{{- $releaseNamespace := .Release.Namespace -}}
{{- $coordinationNamespace := include "ptah-operator.coordinationNamespace" . -}}
{{- $controllerName := include "ptah-operator.fullname" . -}}
{{- $controllerServiceAccount := include "ptah-operator.serviceAccountName" . -}}
{{- $certificateName := include "ptah-operator.certRotatorServiceAccountName" . -}}
{{- $certificateRuntimeEnabled := and .Values.certificateRotation.enabled (not .Values.webhook.existingSecret) -}}
{{- $webhookSecretName := include "ptah-operator.webhookSecretName" . -}}
{{- $webhookServiceName := include "ptah-operator.webhookServiceName" . -}}
{{- $certificateStagingSecretName := include "ptah-operator.certRotationStagingSecretName" . -}}
{{- $hookName := include "ptah-operator.crdManagerServiceAccountName" . -}}
{{- $identities := list
      (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $controllerServiceAccount "source" "controller ServiceAccount")
      (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $hookName "source" "CRD manager hook ServiceAccount")
      (dict "kind" "ClusterRole" "namespace" "" "name" $controllerName "source" "controller ClusterRole")
      (dict "kind" "ClusterRole" "namespace" "" "name" $hookName "source" "CRD manager ClusterRole")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $controllerName "source" "controller ClusterRoleBinding")
      (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $hookName "source" "CRD manager ClusterRoleBinding")
      (dict "kind" "Role" "namespace" $releaseNamespace "name" $hookName "source" "CRD manager Role")
      (dict "kind" "Role" "namespace" $coordinationNamespace "name" $controllerName "source" "controller coordination Role")
      (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $hookName "source" "CRD manager RoleBinding")
      (dict "kind" "RoleBinding" "namespace" $coordinationNamespace "name" $controllerName "source" "controller coordination RoleBinding")
      (dict "kind" "Job" "namespace" $releaseNamespace "name" $hookName "source" "CRD reconcile Job")
-}}
{{- if $certificateRuntimeEnabled -}}
{{- $identities = append $identities (dict "kind" "ServiceAccount" "namespace" $releaseNamespace "name" $certificateName "source" "certificate ServiceAccount") -}}
{{- $identities = append $identities (dict "kind" "ClusterRole" "namespace" "" "name" $certificateName "source" "certificate ClusterRole") -}}
{{- $identities = append $identities (dict "kind" "ClusterRoleBinding" "namespace" "" "name" $certificateName "source" "certificate ClusterRoleBinding") -}}
{{- $identities = append $identities (dict "kind" "Role" "namespace" $releaseNamespace "name" $certificateName "source" "certificate Role") -}}
{{- $identities = append $identities (dict "kind" "RoleBinding" "namespace" $releaseNamespace "name" $certificateName "source" "certificate RoleBinding") -}}
{{- $identities = append $identities (dict "kind" "Secret" "namespace" $releaseNamespace "name" $webhookSecretName "source" "webhook TLS Secret") -}}
{{- $identities = append $identities (dict "kind" "Secret" "namespace" $releaseNamespace "name" $certificateStagingSecretName "source" "certificate staging Secret") -}}
{{- $identities = append $identities (dict "kind" "Service" "namespace" $releaseNamespace "name" $webhookServiceName "source" "webhook Service") -}}
{{- end -}}
{{- if .Values.approverClusterRole.create -}}
{{- $identities = append $identities (dict "kind" "ClusterRole" "namespace" "" "name" (printf "%s-approver" $controllerName) "source" "approver ClusterRole") -}}
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

{{- define "ptah-operator.controllerStateVersion" -}}2{{- end -}}

{{- define "ptah-operator.admissionContractVersion" -}}2{{- end -}}

{{- /* Increase for every published operator release. */ -}}
{{- define "ptah-operator.releaseSequence" -}}1{{- end -}}

{{- define "ptah-operator.hookIdentityDigest" -}}
{{- printf "%s\n%s\n%s\n%s" .Release.Namespace .Release.Name (include "ptah-operator.releaseSequence" .) (include "ptah-operator.managerImage" .) | sha256sum -}}
{{- end -}}

{{- /* The release's own identity: what names its cluster-scoped objects apart
      from another release's, and stays the same across its upgrades. */ -}}
{{- define "ptah-operator.releaseDigest" -}}
{{- printf "%s\n%s" .Release.Namespace .Release.Name | sha256sum | trunc 12 -}}
{{- end -}}

{{- define "ptah-operator.controllerWriteGuardPolicyName" -}}
{{- printf "ptah-operator-controller-write-guard-v2-%s" (include "ptah-operator.releaseDigest" .) -}}
{{- end -}}

{{- define "ptah-operator.controllerJobWriteGuardPolicyName" -}}
{{- printf "ptah-operator-job-write-guard-v2-%s" (include "ptah-operator.releaseDigest" .) -}}
{{- end -}}

{{- define "ptah-operator.controllerChunkWriteGuardPolicyName" -}}
{{- printf "ptah-operator-chunk-write-guard-v2-%s" (include "ptah-operator.releaseDigest" .) -}}
{{- end -}}

{{- define "ptah-operator.controllerPlanWriteGuardPolicyName" -}}
{{- printf "ptah-operator-plan-write-guard-v2-%s" (include "ptah-operator.releaseDigest" .) -}}
{{- end -}}

{{- define "ptah-operator.controllerMigrationPlanWriteGuardPolicyName" -}}
{{- printf "ptah-operator-migration-plan-write-guard-v1-%s" (include "ptah-operator.releaseDigest" .) -}}
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
	  (printf "--controller-service-account-username=system:serviceaccount:%s:%s" .Release.Namespace (include "ptah-operator.serviceAccountName" .))
      (printf "--default-tolerations-enabled=%t" .Values.admission.defaultTolerationsEnabled)
      (printf "--default-not-ready-toleration-seconds=%v" .Values.admission.defaultNotReadyTolerationSeconds)
      (printf "--default-unreachable-toleration-seconds=%v" .Values.admission.defaultUnreachableTolerationSeconds)
      (printf "--extended-resource-toleration-enabled=%t" .Values.admission.extendedResourceTolerationEnabled)
      (printf "--always-pull-images-enabled=%t" .Values.admission.alwaysPullImagesEnabled) -}}
{{- $args | toJson -}}
{{- end -}}

{{- define "ptah-operator.certificateRuntimeArgsJSON" -}}
{{- $rotatorName := include "ptah-operator.certRotatorServiceAccountName" . -}}
{{- $mutatingWebhookNames := "mapproval.operator.ptah.run,mmigrationapproval.operator.ptah.run" -}}
{{- $validatingWebhookNames := "vapproval.operator.ptah.run,vmigrationapproval.operator.ptah.run,vpodintent.operator.ptah.run,vcontrollerwrite.operator.ptah.run" -}}
{{- $args := list
      (printf "--namespace=%s" .Release.Namespace)
      (printf "--release-name=%s" .Release.Name)
      (printf "--secret-name=%s" (include "ptah-operator.webhookSecretName" .))
      (printf "--staging-secret-name=%s" (include "ptah-operator.certRotationStagingSecretName" .)) -}}
{{- if .Values.certificateRotation.recreateMissingSecret -}}
{{- $args = append $args "--recreate-missing-secret=true" -}}
{{- $args = append $args (printf "--secret-create-policy-name=%s" $rotatorName) -}}
{{- $args = append $args (printf "--secret-create-policy-binding-name=%s" $rotatorName) -}}
{{- $args = append $args (printf "--secret-create-service-account-name=%s" $rotatorName) -}}
{{- end -}}
{{- $args = concat $args (list
      (printf "--lease-name=%s" (include "ptah-operator.certRotationLeaseName" .))
      (printf "--mutating-webhook-configuration=%s" (include "ptah-operator.approvalWebhookConfigurationName" .))
      (printf "--mutating-webhook-names=%s" $mutatingWebhookNames)
      (printf "--validating-webhook-configuration=%s" (include "ptah-operator.approvalWebhookConfigurationName" .))
      (printf "--validating-webhook-names=%s" $validatingWebhookNames)
      (printf "--service-name=%s" (include "ptah-operator.webhookServiceName" .))
      (printf "--service-namespace=%s" .Release.Namespace)
      "--endpoint-port-name=https"
      "--holder-identity=$(POD_NAME)/$(POD_UID)"
      (printf "--run-interval=%s" .Values.certificateRotation.interval)
      (printf "--ca-switch-delay=%s" (default .Values.certificateRotation.interval .Values.certificateRotation.caSwitchDelay))
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

{{/*
The CRD reconcile hook's arguments. It stops the release's runtime when the
manager image changes and then brings the CRDs to this release's schemas, so
it needs the two Deployments it may stop and the image that tells whether it
has to.

The release sequence and the controller-state version are the chart's, and the
hook refuses them unless its image compiles the same ones. That is what
refuses a chart paired with another release's image, as a --reuse-values
upgrade that keeps the old image.digest makes, before the hook changes
anything.
*/}}
{{- define "ptah-operator.crdReconcileArgsJSON" -}}
{{- list
      "reconcile"
      "--timeout=360s"
      (printf "--release-name=%s" .Release.Name)
      (printf "--release-namespace=%s" .Release.Namespace)
      (printf "--controller-deployment-name=%s" (include "ptah-operator.fullname" .))
      (printf "--certificate-deployment-name=%s" (include "ptah-operator.certRotatorServiceAccountName" .))
      (printf "--manager-image=%s" (include "ptah-operator.managerImage" .))
      (printf "--release-sequence=%s" (include "ptah-operator.releaseSequence" .))
      (printf "--controller-state-version=%s" (include "ptah-operator.controllerStateVersion" .))
    | toJson -}}
{{- end -}}

{{/*
The arguments of the check each runtime Pod runs before it starts: the CRDs are
this release's, and the admission singleton belongs to this release. The
manager also refuses stored state it cannot read.
*/}}
{{- define "ptah-operator.runtimeVerifyArgsJSON" -}}
{{- $root := .root -}}
{{- $args := list
      "runtime-verify"
      "--timeout=60s"
      (printf "--release-name=%s" $root.Release.Name)
      (printf "--release-namespace=%s" $root.Release.Namespace)
      (printf "--coordination-namespace=%s" (include "ptah-operator.coordinationNamespace" $root))
      (printf "--leader-election=%t" $root.Values.leaderElection)
      (printf "--leader-election-id=%s" (include "ptah-operator.leaderElectionID" $root))
      (printf "--webhook-service-name=%s" (include "ptah-operator.webhookServiceName" $root))
      (printf "--webhook-timeout-seconds=%v" $root.Values.webhook.timeoutSeconds)
      (printf "--hook-service-account-name=%s" (include "ptah-operator.crdManagerServiceAccountName" $root))
      (printf "--controller-service-account-name=%s" (include "ptah-operator.serviceAccountName" $root))
      (printf "--controller-deployment-name=%s" (include "ptah-operator.fullname" $root))
      (printf "--certificate-deployment-name=%s" (include "ptah-operator.certRotatorServiceAccountName" $root))
      (printf "--release-sequence=%s" (include "ptah-operator.releaseSequence" $root)) -}}
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
{{- $expected := merge (dict) .expectedImmutable .expectedVersions (dict "operator.ptah.run/hook-service-account-name" .expectedHook "operator.ptah.run/controller-service-account-name" .expectedController) -}}
{{- range $key := keys $expected -}}
{{- if hasKey $annotations $key -}}
{{- $present = add1 $present -}}
{{- end -}}
{{- end -}}
{{- if ne $present (len $expected) -}}
{{- fail (printf "fixed admission singleton %s/%s has an incomplete owned annotation tuple" .kind .object.metadata.name) -}}
{{- end -}}
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
{{- $actualRelease := index $annotations "operator.ptah.run/release-sequence" -}}
{{- $actualController := index $annotations "operator.ptah.run/controller-service-account-name" -}}
{{- $actualHook := index $annotations "operator.ptah.run/hook-service-account-name" -}}
{{- if eq $actualRelease (index .expectedVersions "operator.ptah.run/release-sequence") -}}
{{- if ne $actualController .expectedController -}}
{{- fail (printf "fixed admission singleton %s/%s annotation operator.ptah.run/controller-service-account-name is %q, expected %q" .kind .object.metadata.name $actualController .expectedController) -}}
{{- end -}}
{{- if ne $actualHook .expectedHook -}}
{{- fail (printf "fixed admission singleton %s/%s annotation operator.ptah.run/hook-service-account-name is %q, expected %q" .kind .object.metadata.name $actualHook .expectedHook) -}}
{{- end -}}
{{- else -}}
{{- if or (eq .expectedPreviousRelease "0") (ne $actualRelease .expectedPreviousRelease) -}}
{{- fail (printf "fixed admission singleton %s/%s has unexpected predecessor release sequence %s" .kind .object.metadata.name $actualRelease) -}}
{{- end -}}
{{- if ne $actualController .expectedController -}}
{{- fail (printf "fixed admission singleton %s/%s annotation operator.ptah.run/controller-service-account-name is %q, expected %q" .kind .object.metadata.name $actualController .expectedController) -}}
{{- end -}}
{{- $currentSuffix := printf `-crd-v%s-[0-9a-f]{12}$` (index .expectedVersions "operator.ptah.run/release-sequence") -}}
{{- $prefix := regexReplaceAll $currentSuffix .expectedHook "-crd-v" -}}
{{- $historicalPattern := printf `^%s%s-[0-9a-f]{12}$` $prefix $actualRelease -}}
{{- if not (regexMatch $historicalPattern $actualHook) -}}
{{- fail (printf "fixed admission singleton %s/%s has invalid historical hook ServiceAccount identity %q" .kind .object.metadata.name $actualHook) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "ptah-operator.validateAdmissionSingleton" -}}
{{- $name := include "ptah-operator.approvalWebhookConfigurationName" . -}}
{{- include "ptah-operator.validateAdmissionSingletonObjects" (dict
      "root" .
      "name" $name
      "mutating" (lookup "admissionregistration.k8s.io/v1" "MutatingWebhookConfiguration" "" $name)
      "validating" (lookup "admissionregistration.k8s.io/v1" "ValidatingWebhookConfiguration" "" $name)) -}}
{{- end -}}

{{/*
The admission singleton check over the two configurations it is handed. The
caller does the lookups, so a render can hand it objects it has to refuse.
*/}}
{{- define "ptah-operator.validateAdmissionSingletonObjects" -}}
{{- $root := .root -}}
{{- $name := .name -}}
{{- $mutating := .mutating -}}
{{- $validating := .validating -}}
{{- if or (and $mutating (not $validating)) (and $validating (not $mutating)) -}}
{{- fail (printf "fixed admission singleton %s is incomplete; both mutating and validating configurations must exist or both must be absent" $name) -}}
{{- end -}}
{{- $expectedImmutable := dict
      "operator.ptah.run/release-name" $root.Release.Name
      "operator.ptah.run/release-namespace" $root.Release.Namespace
      "operator.ptah.run/coordination-namespace" (include "ptah-operator.coordinationNamespace" $root)
      "operator.ptah.run/leader-election" (printf "%t" $root.Values.leaderElection)
      "operator.ptah.run/leader-election-id" (include "ptah-operator.leaderElectionID" $root)
      "operator.ptah.run/webhook-service-name" (include "ptah-operator.webhookServiceName" $root)
      "operator.ptah.run/controller-deployment-name" (include "ptah-operator.fullname" $root)
      "operator.ptah.run/certificate-deployment-name" (include "ptah-operator.certRotatorServiceAccountName" $root) -}}
{{- $expectedVersions := dict
      "operator.ptah.run/controller-state-version" (include "ptah-operator.controllerStateVersion" $root)
      "operator.ptah.run/admission-contract-version" (include "ptah-operator.admissionContractVersion" $root)
      "operator.ptah.run/release-sequence" (include "ptah-operator.releaseSequence" $root) -}}
{{- $context := dict "expectedImmutable" $expectedImmutable "expectedVersions" $expectedVersions "expectedHook" (include "ptah-operator.crdManagerServiceAccountName" $root) "expectedController" (include "ptah-operator.serviceAccountName" $root) "expectedPreviousRelease" (include "ptah-operator.predecessorReleaseSequence" $root) "releaseName" $root.Release.Name "releaseNamespace" $root.Release.Namespace -}}
{{- include "ptah-operator.validateAdmissionSingletonObject" (merge (dict "kind" "MutatingWebhookConfiguration" "object" $mutating) $context) -}}
{{- include "ptah-operator.validateAdmissionSingletonObject" (merge (dict "kind" "ValidatingWebhookConfiguration" "object" $validating) $context) -}}
{{- end -}}

{{/*
ptah-operator.validateReleaseNamespace refuses an install or upgrade into a
namespace the operator would share. The release namespace is part of the
operator's trusted computing base: Kubernetes lets whoever can create a Pod in
a namespace run it as any ServiceAccount there, so a namespace the whole
cluster uses, or one that already runs somebody else's workloads, hands the
operator's identities to principals nobody chose to administer Ptah.

It needs the cluster. A list lookup that reached an API server answers with an
items key, even for a namespace that is empty or not created yet, while helm
template, a client-side dry run and a GitOps render answer with an empty map.
The check runs only in the first case, namespace name included, because an
offline render such as helm lint renders into default.

A workload is this release's when it carries the release's
app.kubernetes.io/instance label, which every workload the chart renders and
every Pod those workloads create carries. This catches a plain mistake and is
not a boundary: grants made through RoleBindings are what NOTES.txt warns
about, and ClusterRoleBindings, external authorizers and grants made later are
outside what a render can see.
*/}}
{{- define "ptah-operator.validateReleaseNamespace" -}}
{{- $namespace := .Release.Namespace -}}
{{- $pods := lookup "v1" "Pod" $namespace "" -}}
{{- if and (hasKey $pods "items") (not .Values.releaseNamespace.allowSharedNamespace) -}}
{{- $advice := printf "whoever can create workloads in the release namespace can run them as the operator's ServiceAccounts, so install into a namespace of its own, or set releaseNamespace.allowSharedNamespace=true if everyone who can create workloads in %s is trusted to administer Ptah" $namespace -}}
{{- if or (eq $namespace "default") (hasPrefix "kube-" $namespace) -}}
{{- fail (printf "release namespace %s is shared with the whole cluster: %s" $namespace $advice) -}}
{{- end -}}
{{- $foreign := list -}}
{{- range $kind := list
      (list "v1" "Pod")
      (list "v1" "ReplicationController")
      (list "apps/v1" "Deployment")
      (list "apps/v1" "StatefulSet")
      (list "apps/v1" "DaemonSet")
      (list "apps/v1" "ReplicaSet")
      (list "batch/v1" "Job")
      (list "batch/v1" "CronJob") -}}
{{- $objects := $pods -}}
{{- if ne (index $kind 1) "Pod" -}}
{{- $objects = lookup (index $kind 0) (index $kind 1) $namespace "" -}}
{{- end -}}
{{- range $object := default (list) $objects.items -}}
{{- $labels := default (dict) $object.metadata.labels -}}
{{- if ne (default "" (index $labels "app.kubernetes.io/instance")) $.Release.Name -}}
{{- $foreign = append $foreign (printf "%s/%s" (index $kind 1) $object.metadata.name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $foreign -}}
{{- $named := sortAlpha $foreign -}}
{{- $shown := join ", " (slice $named 0 (min 5 (len $named))) -}}
{{- if gt (len $named) 5 -}}
{{- $shown = printf "%s and %d more" $shown (sub (len $named) 5) -}}
{{- end -}}
{{- fail (printf "release namespace %s runs workloads without app.kubernetes.io/instance=%s (%s): %s" $namespace $.Release.Name $shown $advice) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
ptah-operator.releaseNamespaceGrantWarnings lists, as JSON, the RoleBindings
in the release namespace and a separate coordination namespace that let a
subject other than this release's ServiceAccounts create what makes its holder
a Ptah administrator: a Pod, an exec session, a ServiceAccount token, a
workload controller or a Lease, or everything admin, edit or cluster-admin
grants. NOTES.txt prints them as a warning and refuses nothing.

A ServiceAccount is this release's when it lives in the release namespace and
carries one of the names the chart runs Pods as, computed by the same helpers
that name them: the manager, the certificate rotator, and the CRD hook. Names
rather than the objects: a first install reads the bindings before Helm has
created any of them. A ClusterRole is read from its rules, which for an
aggregated role are the aggregated rules: the aggregation controller writes
them into the object. An offline render reads nothing and lists nothing.
*/}}
{{- define "ptah-operator.releaseNamespaceGrantWarnings" -}}
{{- $root := . -}}
{{- $namespaces := list .Release.Namespace -}}
{{- $coordination := include "ptah-operator.coordinationNamespace" . -}}
{{- if ne $coordination .Release.Namespace -}}
{{- $namespaces = append $namespaces $coordination -}}
{{- end -}}
{{- $own := dict -}}
{{- range $name := list
      (include "ptah-operator.serviceAccountName" .)
      (include "ptah-operator.certRotatorServiceAccountName" .)
      (include "ptah-operator.crdManagerServiceAccountName" .) -}}
{{- if $name -}}
{{- $_ := set $own $name true -}}
{{- end -}}
{{- end -}}
{{- $warnings := list -}}
{{- range $namespace := $namespaces -}}
{{- $bindings := lookup "rbac.authorization.k8s.io/v1" "RoleBinding" $namespace "" -}}
{{- range $binding := default (list) $bindings.items -}}
{{- $foreign := list -}}
{{- range $subject := default (list) $binding.subjects -}}
{{- $subjectNamespace := default $namespace $subject.namespace -}}
{{- $ownSubject := false -}}
{{- if and (eq $subject.kind "ServiceAccount") (eq $subjectNamespace $root.Release.Namespace) (hasKey $own $subject.name) -}}
{{- $ownSubject = true -}}
{{- end -}}
{{- if not $ownSubject -}}
{{- if eq $subject.kind "ServiceAccount" -}}
{{- $foreign = append $foreign (printf "ServiceAccount %s/%s" $subjectNamespace $subject.name) -}}
{{- else -}}
{{- $foreign = append $foreign (printf "%s %s" $subject.kind $subject.name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $foreign -}}
{{- $roleRef := default (dict) $binding.roleRef -}}
{{- $reason := "" -}}
{{- if and (eq $roleRef.kind "ClusterRole") (has $roleRef.name (list "admin" "edit" "cluster-admin")) -}}
{{- $reason = printf "binds ClusterRole %s" $roleRef.name -}}
{{- else -}}
{{- $role := dict -}}
{{- if eq $roleRef.kind "ClusterRole" -}}
{{- $role = lookup "rbac.authorization.k8s.io/v1" "ClusterRole" "" $roleRef.name -}}
{{- else if eq $roleRef.kind "Role" -}}
{{- $role = lookup "rbac.authorization.k8s.io/v1" "Role" $namespace $roleRef.name -}}
{{- end -}}
{{- $granted := include "ptah-operator.administratorCreateGrants" (default (list) $role.rules) | fromJsonArray -}}
{{- if $granted -}}
{{- $reason = printf "grants create on %s through %s %s" (join ", " $granted) $roleRef.kind $roleRef.name -}}
{{- end -}}
{{- end -}}
{{- if $reason -}}
{{- $warnings = append $warnings (printf "RoleBinding %s/%s %s to %s" $namespace $binding.metadata.name $reason (join ", " $foreign)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $warnings | toJson -}}
{{- end -}}

{{/*
ptah-operator.administratorCreateGrants lists, as JSON, which of the resources
that make their creator a Ptah administrator in the operator's namespaces a
list of RBAC rules lets its holder create. It matches a rule the way the RBAC
authorizer does: the create verb or the verb wildcard, the resource's API
group or the group wildcard, and the resource itself, the resource wildcard,
or, for a subresource, the wildcard that names only the subresource. A rule
that lists resourceNames never authorizes creating a top-level object, whose
name the authorizer does not know yet, but it does authorize a subresource of
the objects it names, so it counts for pods/exec and serviceaccounts/token.
*/}}
{{- define "ptah-operator.administratorCreateGrants" -}}
{{- $targets := list
      (list "" "pods")
      (list "" "pods/exec")
      (list "" "serviceaccounts/token")
      (list "" "replicationcontrollers")
      (list "apps" "deployments")
      (list "apps" "statefulsets")
      (list "apps" "daemonsets")
      (list "apps" "replicasets")
      (list "batch" "jobs")
      (list "batch" "cronjobs")
      (list "coordination.k8s.io" "leases") -}}
{{- $granted := list -}}
{{- range $rule := . -}}
{{- $verbs := default (list) $rule.verbs -}}
{{- if or (has "create" $verbs) (has "*" $verbs) -}}
{{- $groups := default (list) $rule.apiGroups -}}
{{- $resources := default (list) $rule.resources -}}
{{- $named := not (empty $rule.resourceNames) -}}
{{- range $target := $targets -}}
{{- $group := index $target 0 -}}
{{- $resource := index $target 1 -}}
{{- $subresourceWildcard := "" -}}
{{- if contains "/" $resource -}}
{{- $subresourceWildcard = printf "*/%s" (last (splitList "/" $resource)) -}}
{{- end -}}
{{- if and
      (or (not $named) $subresourceWildcard)
      (or (has "*" $groups) (has $group $groups))
      (or (has "*" $resources) (has $resource $resources) (and $subresourceWildcard (has $subresourceWildcard $resources))) -}}
{{- $name := $resource -}}
{{- if $group -}}
{{- $name = printf "%s.%s" $resource $group -}}
{{- end -}}
{{- if not (has $name $granted) -}}
{{- $granted = append $granted $name -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $granted | toJson -}}
{{- end -}}

{{- /* bootstrapCADays is the life of the CA webhookCertificateMaterialJSON
      generates when no webhook Secret exists yet.
      validateCertificateRenewalThreshold holds the renewal threshold to at
      least this, so both read it here and cannot drift apart. */ -}}
{{- define "ptah-operator.bootstrapCADays" -}}2{{- end -}}

{{/*
The certificate material the webhook Secret carries: read from the Secret the
release already holds, or generated when it holds none. The caller does the
lookup and passes the result as .existing, so a render can hand this helper a
Secret it has to refuse.
*/}}
{{- define "ptah-operator.webhookCertificateMaterialJSON" -}}
{{- $root := .root -}}
{{- $existing := .existing -}}
{{- $material := dict "caBundle" "" "caKey" "" "tlsCrt" "" "tlsKey" "" -}}
{{- if $root.Values.webhook.existingSecret -}}
{{- if $root.Values.webhook.caBundle -}}
{{- $_ := set $material "caBundle" ($root.Values.webhook.caBundle | b64enc) -}}
{{- else if $existing -}}
{{- $_ := set $material "caBundle" (required "webhook.existingSecret must contain ca.crt" (index $existing.data "ca.crt")) -}}
{{- else -}}
{{- fail "webhook.caBundle is required when webhook.existingSecret cannot be read" -}}
{{- end -}}
{{- else if $existing -}}
{{- $expectedLabels := dict
      "app.kubernetes.io/managed-by" "Helm"
      "operator.ptah.run/generated-webhook-certificate" "true" -}}
{{- $expectedAnnotations := dict
      "meta.helm.sh/release-name" $root.Release.Name
      "meta.helm.sh/release-namespace" $root.Release.Namespace -}}
{{- if or
      (not (deepEqual (default (dict) $existing.metadata.labels) $expectedLabels))
      (not (deepEqual (default (dict) $existing.metadata.annotations) $expectedAnnotations)) -}}
{{- fail "generated webhook Secret has foreign or incomplete Helm ownership metadata" -}}
{{- end -}}
{{- $_ := set $material "caBundle" (required "generated webhook Secret must contain ca.crt" (index $existing.data "ca.crt")) -}}
{{- $_ := set $material "caKey" (required "generated webhook Secret must contain ca.key" (index $existing.data "ca.key")) -}}
{{- $_ := set $material "tlsCrt" (required "generated webhook Secret must contain tls.crt" (index $existing.data "tls.crt")) -}}
{{- $_ := set $material "tlsKey" (required "generated webhook Secret must contain tls.key" (index $existing.data "tls.key")) -}}
{{- else -}}
{{- /* Bootstrap material is deliberately short-lived. The rotator promptly replaces it with certificates matching the configured policy. */ -}}
{{- $ca := genCA (printf "%s-ca" (include "ptah-operator.fullname" $root)) (int (include "ptah-operator.bootstrapCADays" $root)) -}}
{{- $service := include "ptah-operator.webhookServiceName" $root -}}
{{- $dnsNames := list $service (printf "%s.%s" $service $root.Release.Namespace) (printf "%s.%s.svc" $service $root.Release.Namespace) (printf "%s.%s.svc.cluster.local" $service $root.Release.Namespace) -}}
{{- $cert := genSignedCert $service nil $dnsNames 1 $ca -}}
{{- $_ := set $material "caBundle" ($ca.Cert | b64enc) -}}
{{- $_ := set $material "caKey" ($ca.Key | b64enc) -}}
{{- $_ := set $material "tlsCrt" ($cert.Cert | b64enc) -}}
{{- $_ := set $material "tlsKey" ($cert.Key | b64enc) -}}
{{- end -}}
{{- $material | toJson -}}
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
{{- /* Neither key is a chart value: the schema refuses both, and a render
      that skips schema validation still refuses them by name rather than
      letting a stale values file select a mode the guards cannot admit. */ -}}
{{- if .Values.image.allowMutableTag -}}
{{- fail "image.allowMutableTag is not a chart value; use image.digest with a registry manifest digest, including for local registries" -}}
{{- end -}}
{{- if .Values.image.testIdentityDigest -}}
{{- fail "image.testIdentityDigest is not a chart value; use image.digest with a registry manifest digest, not a Docker image ID" -}}
{{- end -}}
{{- if not (regexMatch `^sha256:[0-9a-f]{64}$` (default "" .Values.image.digest)) -}}
{{- fail "image.digest must pin the manager with sha256:<64 lowercase hex>; use a registry manifest digest, including for local registries" -}}
{{- end -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- end -}}

{{- define "ptah-operator.controllerImage" -}}
{{- include "ptah-operator.managerImage" . -}}
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

{{- /* durationSeconds turns a chart duration ("720h", "15m", "30s", "500ms")
      into whole seconds, rounding milliseconds down. The schema already
      holds every duration value to that shape. */ -}}
{{- define "ptah-operator.durationSeconds" -}}
{{- $value := toString . -}}
{{- $number := regexFind "^[0-9]+" $value -}}
{{- $unit := trimPrefix $number $value -}}
{{- if eq $unit "h" -}}{{- mul (atoi $number) 3600 -}}
{{- else if eq $unit "m" -}}{{- mul (atoi $number) 60 -}}
{{- else if eq $unit "s" -}}{{- atoi $number -}}
{{- else if eq $unit "ms" -}}{{- div (atoi $number) 1000 -}}
{{- else -}}{{- fail (printf "%q is not a chart duration" $value) -}}
{{- end -}}
{{- end -}}

{{- /* The rotator replaces a CA issued inside the renewal threshold in its
      first pass, before it reports ready, so `helm install --wait` covers the
      replacement of the bootstrap CA webhookCertificateMaterialJSON generates.
      A shorter threshold would put that replacement behind the CA switch
      delay, after the install returns, where its last write to the webhook
      bundles can land inside a later upgrade and fail its server-side apply.

      The check reads only values. The helper generates bootstrap material
      only when its lookup finds no Secret, which `helm template` always sees
      and an upgrade never does, so a refusal there would pass the same
      values in one and refuse them in the other. */ -}}
{{- define "ptah-operator.validateCertificateRenewalThreshold" -}}
{{- if and .Values.certificateRotation.enabled (not .Values.webhook.existingSecret) -}}
{{- $bootstrapCAHours := mul (int (include "ptah-operator.bootstrapCADays" .)) 24 -}}
{{- if lt (int (include "ptah-operator.durationSeconds" .Values.certificateRotation.renewalThreshold)) (mul $bootstrapCAHours 3600) -}}
{{- fail (printf "certificateRotation.renewalThreshold must be at least %dh, the lifetime of the bootstrap CA the chart renders, so the rotator replaces that CA before it reports ready" $bootstrapCAHours) -}}
{{- end -}}
{{- end -}}
{{- end -}}

