---
title: Configuration
description: What the chart takes, what it requires, and what it deliberately has no default for.
---

Every setting below is a Helm value on `charts/ptah-operator`. Three of them
have no default and the chart refuses to render without them, because each
names something the operator must not guess.

## The three values with no default

`image.digest` is the manager's registry manifest digest. A tag is not
accepted, and a Docker image ID is not a registry manifest digest.

`execution.executorImage` is the Ptah build the operator runs, by digest.
`execution.runnerImage` is the runner beside it, also by digest.

`execution.ptahVersion` is the identity of the build inside that executor
digest. It is verified from the image's own provenance and never inferred from
a tag, so the chart has no default to fall back on. Which build has actually
been run against this operator is [Ptah compatibility](../support/ptah.md).

All three are recorded in every plan, approval, Job and applied status, so a
reader of a converged schema can see exactly what produced it.

## Every value

<!-- BEGIN GENERATED VALUES -->
| Value | Default | What it does |
| --- | --- | --- |
| `replicaCount` | `2` |  |
| `image` |  |  |
| `image.repository` | `ghcr.io/stokaro/ptah-operator` |  |
| `image.tag` | `"0.1.0"` |  |
| `image.digest` | `""` | Required registry manifest digest, including for local development. A Docker image ID is not a registry manifest digest. |
| `image.pullPolicy` | `IfNotPresent` |  |
| `imagePullSecrets` | `[]` |  |
| `nameOverride` | `""` |  |
| `fullnameOverride` | `""` |  |
| `execution` |  |  |
| `execution.executorImage` | `""` | Both execution images are required and must include @sha256:<64 hex>. ptahVersion is also required, is limited to 128 bytes, and must identify the Ptah build verified for the exact executorImage digest; the chart deliberately has no default. |
| `execution.runnerImage` | `""` |  |
| `execution.ptahVersion` | `""` |  |
| `admission` |  |  |
| `admission.defaultTolerationsEnabled` | `true` | These values must match kube-apiserver's enabled admission plugins and DefaultTolerationSeconds flags. Zero means an exact zero-second NoExecute toleration; it does not disable the plugin. |
| `admission.defaultNotReadyTolerationSeconds` | `300` |  |
| `admission.defaultUnreachableTolerationSeconds` | `300` |  |
| `admission.extendedResourceTolerationEnabled` | `false` |  |
| `admission.alwaysPullImagesEnabled` | `false` |  |
| `leaderElection` | `true` | Required whenever replicaCount is greater than one. Disable only for an isolated single-replica installation. Exactly one Helm release is supported per cluster; scale this release for high availability. |
| `coordination` |  |  |
| `coordination.namespace` | `""` | Leave empty to use the Helm release namespace. This namespace contains both database target Leases and the manager leader-election Lease for the cluster's one supported operator release. |
| `serviceAccount` |  |  |
| `serviceAccount.create` | `true` | With create=false, name is a stable base, not a complete object name. For every release sequence N, pre-create the dedicated ServiceAccount <name>-v<N>. The chart never reuses or deletes these user-owned identities. Any additional RoleBinding or ClusterRoleBinding for an active or retained epoch blocks fail-closed upgrade and uninstall. |
| `serviceAccount.name` | `""` |  |
| `serviceAccount.annotations` | `{}` |  |
| `podAnnotations` | `{}` |  |
| `podLabels` | `{}` |  |
| `resources` |  |  |
| `resources.requests` |  |  |
| `resources.requests.cpu` | `50m` |  |
| `resources.requests.memory` | `96Mi` |  |
| `resources.limits` |  |  |
| `resources.limits.memory` | `256Mi` |  |
| `nodeSelector` | `{}` |  |
| `tolerations` | `[]` |  |
| `affinity` | `{}` |  |
| `priorityClassName` | `""` | Assign this class to the runtime Deployments and to the weight-zero CRD reconcile hook after preflight verifies the live class. Earlier bootstrap, preflight, and uninstall hooks remain classless; Pod admission may apply the cluster's global default class to those hooks. |
| `priorityClassValue` | `0` | Pin the scheduling semantics of priorityClassName. Use 0 and PreemptLowerPriority when no class is configured. The preflight refuses a named class whose live value or effective preemption policy differs, so a PriorityClass change cannot silently alter an approved runtime contract. |
| `priorityClassPreemptionPolicy` | `PreemptLowerPriority` |  |
| `metrics` |  |  |
| `metrics.bindAddress` | `":8080"` |  |
| `metrics.service` |  |  |
| `metrics.service.enabled` | `true` |  |
| `metrics.service.port` | `8080` |  |
| `webhook` |  |  |
| `webhook.port` | `9443` |  |
| `webhook.timeoutSeconds` | `5` |  |
| `webhook.controllerWriteTimeoutSeconds` | `30` | The manager-write validator may read one schema, one plan, and up to 16 immutable plan chunks. Kubernetes caps admission timeouts at 30 seconds; keep this fail-closed path at that bound independently of the fast paths. |
| `webhook.existingSecret` | `""` | Name of a pre-provisioned kubernetes.io/tls Secret. The Secret must also contain ca.crt. Set caBundle to render the chart without cluster lookup. |
| `webhook.caBundle` | `""` | PEM-encoded CA certificate for existingSecret. Leave empty during a connected Helm install to read ca.crt from that Secret. |
| `certificateRotation` |  |  |
| `certificateRotation.enabled` | `true` | Built-in management is automatically omitted when webhook.existingSecret is set. The manager never receives Secret-read permission. |
| `certificateRotation.recreateMissingSecret` | `false` | Opt in to recreating a deleted chart-generated Secret. This necessarily grants the rotator namespace-wide Secret CREATE in RBAC; a fail-closed admission policy narrows its use to the exact generated TLS Secret. |
| `certificateRotation.interval` | `"6h"` |  |
| `certificateRotation.operationTimeout` | `"15m"` |  |
| `certificateRotation.retryInitial` | `"5s"` |  |
| `certificateRotation.retryMax` | `"5m"` |  |
| `certificateRotation.healthPort` | `8081` |  |
| `certificateRotation.candidatePort` | `9444` |  |
| `certificateRotation.admissionConvergence` |  | A transition is accepted only after every directly addressed API server observes both canary webhooks continuously for this stability window. |
| `certificateRotation.admissionConvergence.stabilityDuration` | `"10s"` |  |
| `certificateRotation.admissionConvergence.pollInterval` | `"1s"` |  |
| `certificateRotation.admissionConvergence.requestTimeout` | `"5s"` | Bounds one endpoint observation: marker GET plus both denial probes. |
| `certificateRotation.renewalThreshold` | `"720h"` |  |
| `certificateRotation.servingCertificateValidity` | `"2160h"` |  |
| `certificateRotation.caCertificateValidity` | `"26280h"` |  |
| `certificateRotation.probeTimeout` | `"5m"` |  |
| `certificateRotation.probeInterval` | `"2s"` |  |
| `certificateRotation.leaseDuration` | `"10m"` |  |
| `certificateRotation.leaseAcquireTimeout` | `"30s"` |  |
| `certificateRotation.resources` |  |  |
| `certificateRotation.resources.requests` |  |  |
| `certificateRotation.resources.requests.cpu` | `10m` |  |
| `certificateRotation.resources.requests.memory` | `32Mi` |  |
| `certificateRotation.resources.limits` |  |  |
| `certificateRotation.resources.limits.memory` | `64Mi` |  |
| `approverClusterRole` |  |  |
| `approverClusterRole.create` | `true` |  |
| `podDisruptionBudget` |  |  |
| `podDisruptionBudget.enabled` | `true` |  |
| `podDisruptionBudget.minAvailable` | `1` |  |
<!-- END GENERATED VALUES -->

The table is generated from `charts/ptah-operator/values.yaml`, which is where
each of these is declared. Run `npm run values:write` in `docs/site` after
changing a value, and the documentation gate refuses a stale table.
