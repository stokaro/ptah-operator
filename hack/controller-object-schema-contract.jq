. as $batch |
($core |
  if length == 1 then .[0]
  else error("core OpenAPI input must contain exactly one document") end) as $core_document |

def require_reviewed_properties($document; $schema_name; $allowed):
  ($document.components.schemas[$schema_name] //
    error("OpenAPI schema is missing: " + $schema_name)) as $schema |
  ($schema.properties //
    error("OpenAPI schema has no properties: " + $schema_name)) as $properties |
  ($properties | keys) as $actual |
  ($actual - $allowed) as $unexpected |
  if ($unexpected | length) == 0 then true
  else error("OpenAPI properties exceed the reviewed controller Job surface for " +
    $schema_name + ": unexpected=" + ($unexpected | tojson))
  end;

if ([$minor == "1.35", $minor == "1.36", $minor == "1.37"] | any) then true
else error("unsupported Kubernetes minor for controller Job schema review: " + $minor)
end and
require_reviewed_properties(
  $batch;
  "io.k8s.api.batch.v1.JobSpec";
  [
    "activeDeadlineSeconds",
    "backoffLimit",
    "backoffLimitPerIndex",
    "completionMode",
    "completions",
    "managedBy",
    "manualSelector",
    "maxFailedIndexes",
    "parallelism",
    "podFailurePolicy",
    "podReplacementPolicy",
    "scheduling",
    "selector",
    "successPolicy",
    "suspend",
    "template",
    "ttlSecondsAfterFinished"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.PodTemplateSpec";
  ["metadata", "spec"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.PodSpec";
  [
    "activeDeadlineSeconds",
    "affinity",
    "automountServiceAccountToken",
    "containers",
    "dnsConfig",
    "dnsPolicy",
    "enableServiceLinks",
    "ephemeralContainers",
    "evictionResponders",
    "hostAliases",
    "hostIPC",
    "hostNetwork",
    "hostPID",
    "hostUsers",
    "hostname",
    "hostnameOverride",
    "imagePullSecrets",
    "initContainers",
    "nodeName",
    "nodeSelector",
    "os",
    "overhead",
    "preemptionPolicy",
    "priority",
    "priorityClassName",
    "readinessGates",
    "resourceClaims",
    "resources",
    "restartPolicy",
    "runtimeClassName",
    "schedulerName",
    "schedulingGates",
    "schedulingGroup",
    "securityContext",
    "serviceAccount",
    "serviceAccountName",
    "setHostnameAsFQDN",
    "shareProcessNamespace",
    "subdomain",
    "terminationGracePeriodSeconds",
    "tolerations",
    "topologySpreadConstraints",
    "volumes",
    "workloadRef"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.Container";
  [
    "args",
    "command",
    "env",
    "envFrom",
    "image",
    "imagePullPolicy",
    "lifecycle",
    "livenessProbe",
    "name",
    "ports",
    "readinessProbe",
    "resizePolicy",
    "resources",
    "restartPolicy",
    "restartPolicyRules",
    "securityContext",
    "startupProbe",
    "stdin",
    "stdinOnce",
    "terminationMessagePath",
    "terminationMessagePolicy",
    "tty",
    "volumeDevices",
    "volumeMounts",
    "workingDir"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.HTTPGetAction";
  ["host", "httpHeaders", "path", "port", "protocol", "scheme"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.GRPCAction";
  ["mode", "port", "service"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.SecurityContext";
  [
    "allowPrivilegeEscalation",
    "appArmorProfile",
    "capabilities",
    "privileged",
    "procMount",
    "readOnlyRootFilesystem",
    "runAsGroup",
    "runAsNonRoot",
    "runAsUser",
    "seLinuxOptions",
    "seccompProfile",
    "windowsOptions"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.PodSecurityContext";
  [
    "appArmorProfile",
    "fsGroup",
    "fsGroupChangePolicy",
    "runAsGroup",
    "runAsNonRoot",
    "runAsUser",
    "seLinuxChangePolicy",
    "seLinuxOptions",
    "seccompProfile",
    "supplementalGroups",
    "supplementalGroupsPolicy",
    "sysctls",
    "windowsOptions"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.Volume";
  [
    "awsElasticBlockStore",
    "azureDisk",
    "azureFile",
    "cephfs",
    "cinder",
    "configMap",
    "csi",
    "downwardAPI",
    "emptyDir",
    "ephemeral",
    "fc",
    "flexVolume",
    "flocker",
    "gcePersistentDisk",
    "gitRepo",
    "glusterfs",
    "hostPath",
    "image",
    "iscsi",
    "name",
    "nfs",
    "persistentVolumeClaim",
    "photonPersistentDisk",
    "portworxVolume",
    "projected",
    "quobyte",
    "rbd",
    "scaleIO",
    "secret",
    "storageos",
    "vsphereVolume"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.EmptyDirVolumeSource";
  ["medium", "mode", "sizeLimit"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ConfigMapVolumeSource";
  ["defaultMode", "defaultUser", "items", "name", "optional"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.SecretVolumeSource";
  ["defaultMode", "defaultUser", "items", "optional", "secretName"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ProjectedVolumeSource";
  ["defaultMode", "defaultUser", "sources"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.VolumeProjection";
  [
    "clusterTrustBundle",
    "configMap",
    "downwardAPI",
    "podCertificate",
    "secret",
    "serviceAccountToken"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ServiceAccountTokenProjection";
  ["audience", "expirationSeconds", "path", "user"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ClusterTrustBundleProjection";
  ["labelSelector", "name", "optional", "path", "signerName", "user"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.PodCertificateProjection";
  [
    "certificateChainPath",
    "credentialBundlePath",
    "keyPath",
    "keyType",
    "maxExpirationSeconds",
    "signerName",
    "user",
    "userAnnotations"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ConfigMapProjection";
  ["items", "name", "optional"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.SecretProjection";
  ["items", "name", "optional"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.DownwardAPIProjection";
  ["items"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.DownwardAPIVolumeSource";
  ["defaultMode", "defaultUser", "items"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.DownwardAPIVolumeFile";
  ["fieldRef", "mode", "path", "resourceFieldRef", "user"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.KeyToPath";
  ["key", "mode", "path", "user"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.VolumeMount";
  [
    "bindMountOptions",
    "mountPath",
    "mountPropagation",
    "name",
    "readOnly",
    "recursiveReadOnly",
    "subPath",
    "subPathExpr"
  ]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ResourceRequirements";
  ["claims", "limits", "requests"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.Capabilities";
  ["add", "drop"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.SeccompProfile";
  ["localhostProfile", "type"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.EnvVar";
  ["name", "value", "valueFrom"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.EnvVarSource";
  ["configMapKeyRef", "fieldRef", "fileKeyRef", "resourceFieldRef", "secretKeyRef"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.EnvFromSource";
  ["configMapRef", "prefix", "secretRef"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.SecretKeySelector";
  ["key", "name", "optional"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.ConfigMapKeySelector";
  ["key", "name", "optional"]
) and
require_reviewed_properties(
  $core_document;
  "io.k8s.api.core.v1.Toleration";
  ["effect", "key", "operator", "tolerationSeconds", "value"]
)
