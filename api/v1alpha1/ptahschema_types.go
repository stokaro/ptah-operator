package v1alpha1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DatabaseEngine is an explicitly supported database family. Requiring it
// lets the controller reject unsupported targets without reading a credential.
// +kubebuilder:validation:Enum=PostgreSQL;MySQL
type DatabaseEngine string

const (
	DatabaseEnginePostgreSQL DatabaseEngine = "PostgreSQL"
	DatabaseEngineMySQL      DatabaseEngine = "MySQL"
)

// ApplyPolicy controls when a current, non-stale plan may execute.
// +kubebuilder:validation:Enum=Never;OnApproval;Always
type ApplyPolicy string

const (
	ApplyPolicyNever      ApplyPolicy = "Never"
	ApplyPolicyOnApproval ApplyPolicy = "OnApproval"
	ApplyPolicyAlways     ApplyPolicy = "Always"
)

// ReconciliationPhase is the externally visible state-machine phase.
// +kubebuilder:validation:Enum=Pending;Resolving;Verifying;Observing;Planning;ReadyToApply;AwaitingApproval;Blocked;Applying;VerifyingConvergence;InSync;Suspended;Failed
type ReconciliationPhase string

const (
	PhasePending              ReconciliationPhase = "Pending"
	PhaseResolving            ReconciliationPhase = "Resolving"
	PhaseVerifying            ReconciliationPhase = "Verifying"
	PhaseObserving            ReconciliationPhase = "Observing"
	PhasePlanning             ReconciliationPhase = "Planning"
	PhaseReadyToApply         ReconciliationPhase = "ReadyToApply"
	PhaseAwaitingApproval     ReconciliationPhase = "AwaitingApproval"
	PhaseBlocked              ReconciliationPhase = "Blocked"
	PhaseApplying             ReconciliationPhase = "Applying"
	PhaseVerifyingConvergence ReconciliationPhase = "VerifyingConvergence"
	PhaseInSync               ReconciliationPhase = "InSync"
	PhaseSuspended            ReconciliationPhase = "Suspended"
	PhaseFailed               ReconciliationPhase = "Failed"
)

// OperationType identifies one serialized execution Job.
// +kubebuilder:validation:Enum=Resolve;Verify;Observe;Plan;Apply
type OperationType string

const (
	OperationResolve OperationType = "Resolve"
	OperationVerify  OperationType = "Verify"
	OperationObserve OperationType = "Observe"
	OperationPlan    OperationType = "Plan"
	OperationApply   OperationType = "Apply"
)

// RegistryAuthMode selects one standard Kubernetes Secret representation.
// +kubebuilder:validation:Enum=Environment;DockerConfigJSON
type RegistryAuthMode string

const (
	RegistryAuthEnvironment      RegistryAuthMode = "Environment"
	RegistryAuthDockerConfigJSON RegistryAuthMode = "DockerConfigJSON"

	// RegistryAuthoritySecretKey is the fixed Secret key by which a credential
	// owner grants the operator permission to use either supported credential
	// representation for one exact OCI registry authority. It is intentionally
	// not selectable by a PtahSchema author.
	RegistryAuthoritySecretKey = "registry"
	// RegistryAllowPlainHTTPSecretKey is the fixed Secret key by which a
	// credential owner explicitly permits that credential to cross an
	// unencrypted registry connection. Its value must be exactly "true".
	RegistryAllowPlainHTTPSecretKey = "allowPlainHTTP"
	// RegistryCASHA256SecretKey is the fixed Secret key by which the same
	// credential owner grants one exact custom CA bundle. Its value must be the
	// lowercase SHA-256 digest of the selected ConfigMap bytes. It is
	// intentionally not selectable by a PtahSchema author.
	RegistryCASHA256SecretKey = "caSHA256"
)

// PtahSchemaSpec declares one desired schema and one database target.
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.connectTimeout) || !has(self.execution.activeDeadlineSeconds) || duration(self.execution.connectTimeout).getMilliseconds() <= self.execution.activeDeadlineSeconds * 1000",message="execution.connectTimeout must not exceed execution.activeDeadlineSeconds"
// +kubebuilder:validation:XValidation:rule="!has(self.policy) || !has(self.policy.lockTimeout) || !has(self.execution) || !has(self.execution.activeDeadlineSeconds) || duration(self.policy.lockTimeout).getMilliseconds() <= self.execution.activeDeadlineSeconds * 1000",message="policy.lockTimeout must not exceed execution.activeDeadlineSeconds"
// +kubebuilder:validation:XValidation:rule="self.target.urlFrom.name.size() > 0 && self.target.urlFrom.key.size() > 0 && (!has(self.target.urlFrom.optional) || !self.target.urlFrom.optional)",message="target.urlFrom must name a required Secret key"
// +kubebuilder:validation:XValidation:rule="self.desired.verificationPolicyFrom.name.size() > 0 && self.desired.verificationPolicyFrom.key.size() > 0 && (!has(self.desired.verificationPolicyFrom.optional) || !self.desired.verificationPolicyFrom.optional)",message="desired.verificationPolicyFrom must name a required ConfigMap key"
// +kubebuilder:validation:XValidation:rule="!has(self.dev) || (self.dev.urlFrom.name.size() > 0 && self.dev.urlFrom.key.size() > 0 && (!has(self.dev.urlFrom.optional) || !self.dev.urlFrom.optional))",message="dev.urlFrom must name a required Secret key"
// +kubebuilder:validation:XValidation:rule="!has(self.desired.transport) || !has(self.desired.transport.caFrom) || (self.desired.transport.caFrom.name.size() > 0 && self.desired.transport.caFrom.key.size() > 0 && (!has(self.desired.transport.caFrom.optional) || !self.desired.transport.caFrom.optional))",message="desired.transport.caFrom must name a required ConfigMap key"
type PtahSchemaSpec struct {
	Target  DatabaseTargetSpec    `json:"target"`
	Desired OCIArtifactSourceSpec `json:"desired"`
	Dev     *DatabaseTargetRef    `json:"dev,omitempty"`
	Policy  ReconciliationPolicy  `json:"policy,omitempty"`

	// Interval is the cadence for resolving mutable tags and observing drift.
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s') && duration(self) <= duration('24h')",message="interval must be between 10s and 24h"
	Interval metav1.Duration `json:"interval,omitempty"`

	Execution ExecutionSpec `json:"execution,omitempty"`

	// Suspend prevents new Jobs. A Job already applying is observed to a
	// terminal result and is never replaced by a destructive cleanup action.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// DatabaseTargetSpec identifies a supported engine, a namespaced Secret key,
// and the stable coordination realm shared by every route to the same physical
// database. There is deliberately no namespace field.
type DatabaseTargetSpec struct {
	Engine DatabaseEngine `json:"engine"`

	// CoordinationKey is a non-secret, stable identifier for the physical
	// database realm. Every schema that can reach the same database through an
	// alias, proxy, or different credential must use exactly the same key.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9](?:[a-z0-9._:/-]{0,251}[a-z0-9])?$`
	CoordinationKey string `json:"coordinationKey"`

	URLFrom corev1.SecretKeySelector `json:"urlFrom"`
}

// DatabaseTargetBinding is the key-free immutable target snapshot stored in
// status. CoordinationKey must never be copied into status; its digest is
// persisted separately.
type DatabaseTargetBinding struct {
	Engine  DatabaseEngine           `json:"engine"`
	URLFrom corev1.SecretKeySelector `json:"urlFrom"`
}

// DatabaseTargetRef is a database URL reference used for optional rehearsal.
type DatabaseTargetRef struct {
	URLFrom corev1.SecretKeySelector `json:"urlFrom"`
}

// OCIArtifactSourceSpec is the reusable, credential-isolated OCI source shape.
// The schema and future versioned-migration APIs intentionally reuse the
// transport contract while keeping their reconciliation lifecycles separate.
type OCIArtifactSourceSpec struct {
	// OCIRef is a desired-schema artifact reference. It may name a tag or a
	// digest; every later operation receives only the resolved digest.
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:Pattern=`^oci://[^[:space:]?#]+$`
	OCIRef string `json:"ociRef"`

	RegistryAuthFrom *RegistryAuthSource `json:"registryAuthFrom,omitempty"`

	VerificationPolicyFrom corev1.ConfigMapKeySelector `json:"verificationPolicyFrom"`

	Transport OCITransportSpec `json:"transport,omitempty"`
}

// OCIArtifactAccessBinding is a credential-free, immutable snapshot of the
// exact artifact and Kubernetes credential selectors needed to fetch it. It is
// persisted across post-Apply proof so a newer generation cannot send newly
// selected credentials to the old artifact's registry.
type OCIArtifactAccessBinding struct {
	ResolvedReference string              `json:"resolvedReference"`
	Digest            string              `json:"digest"`
	RegistryAuthFrom  *RegistryAuthSource `json:"registryAuthFrom,omitempty"`
	Transport         OCITransportSpec    `json:"transport,omitempty"`
}

// DesiredSchemaSpec is retained as a source-compatible name for clients that
// used the provisional API type before the common OCI source was factored out.
type DesiredSchemaSpec = OCIArtifactSourceSpec

// RegistryAuthSource describes a Secret without requiring the controller to
// read it. The kubelet projects only the selected credential representation
// into a Job, while every mode also projects the fixed registry authority grant
// to the runner.
// +kubebuilder:validation:XValidation:rule="self.mode != 'DockerConfigJSON' || has(self.dockerConfigJSONKey)",message="dockerConfigJSONKey is required in DockerConfigJSON mode"
type RegistryAuthSource struct {
	Name string `json:"name"`

	// +kubebuilder:default=Environment
	Mode RegistryAuthMode `json:"mode,omitempty"`

	// Environment mode supports username/password or an identity token. Keys
	// are optional so a single Secret shape can use either credential form.
	// +kubebuilder:default=username
	UsernameKey string `json:"usernameKey,omitempty"`
	// +kubebuilder:default=password
	PasswordKey string `json:"passwordKey,omitempty"`
	// +kubebuilder:default=token
	TokenKey string `json:"tokenKey,omitempty"`
	// RegistryKey is retained for source compatibility. The key is fixed so the
	// Secret owner, rather than a PtahSchema author, controls the authority grant.
	// The referenced Secret must contain an authority-only host[:port] value.
	// +kubebuilder:default=registry
	// +kubebuilder:validation:Enum=registry
	RegistryKey string `json:"registryKey,omitempty"`

	// +kubebuilder:default=.dockerconfigjson
	DockerConfigJSONKey string `json:"dockerConfigJSONKey,omitempty"`
}

// OCITransportSpec configures private and air-gapped registries without
// allowing arbitrary files or commands into the execution Pod.
// +kubebuilder:validation:XValidation:rule="!self.plainHTTP || !has(self.caFrom)",message="caFrom cannot be used with plainHTTP"
// +kubebuilder:validation:XValidation:rule="!self.plainHTTP || !has(self.clientCertificateFrom)",message="clientCertificateFrom cannot be used with plainHTTP"
// +kubebuilder:validation:XValidation:rule="!has(self.clientCertificateFrom)",message="clientCertificateFrom is not supported until the executor can scope client certificates across redirects"
type OCITransportSpec struct {
	// PlainHTTP is intended only for explicitly trusted test or air-gapped
	// networks. HTTPS remains the default. When registryAuthFrom is present, its
	// Secret must also contain allowPlainHTTP with the exact value "true".
	// +kubebuilder:default=false
	PlainHTTP bool `json:"plainHTTP,omitempty"`

	// CAFrom selects a custom CA bundle. When registryAuthFrom is present, that
	// same Secret must contain caSHA256 with the exact lowercase SHA-256 digest
	// of the selected bytes.
	CAFrom *corev1.ConfigMapKeySelector `json:"caFrom,omitempty"`

	// ClientCertificateFrom is reserved for a future executor contract that can
	// select a client certificate by the effective TLS authority on every
	// request, including redirects. The current API rejects this field.
	ClientCertificateFrom *TLSSecretReference `json:"clientCertificateFrom,omitempty"`
}

// TLSSecretReference retains the client-certificate selector shape for source
// compatibility. OCITransportSpec currently rejects its use.
type TLSSecretReference struct {
	Name string `json:"name"`
	// +kubebuilder:default=tls.crt
	CertificateKey string `json:"certificateKey,omitempty"`
	// +kubebuilder:default=tls.key
	PrivateKeyKey string `json:"privateKeyKey,omitempty"`
}

// ReconciliationPolicy defines safety decisions. Destructive plans always
// need both allowDestructive=true and a matching approval, even in Always mode.
type ReconciliationPolicy struct {
	// +kubebuilder:default=OnApproval
	Apply ApplyPolicy `json:"apply,omitempty"`

	// +kubebuilder:default=false
	AllowDestructive bool `json:"allowDestructive,omitempty"`

	// +kubebuilder:validation:Enum=all;destructive
	// +kubebuilder:default=all
	DriftSeverity string `json:"driftSeverity,omitempty"`

	// Exclude defines the single authoritative managed scope. Raw drift is
	// observed without exclusions, then a read-only plan classifies this exact
	// scope as changed or converged.
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[^\p{Z}\x00-\x20\x7f\x{0085}](?:[^\x00-\x1f\x7f]*[^\p{Z}\x00-\x20\x7f\x{0085}])?$`
	Exclude []string `json:"exclude,omitempty"`

	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('10m')",message="lockTimeout must be between 1s and 10m"
	LockTimeout metav1.Duration `json:"lockTimeout,omitempty"`

	// +kubebuilder:validation:Enum=all;file;none
	// +kubebuilder:default=file
	TransactionMode string `json:"transactionMode,omitempty"`
}

// ExecutionSpec exposes bounded scheduling and resource controls while
// withholding arbitrary Pod/container command customization.
type ExecutionSpec struct {
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=900
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`

	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5s') && duration(self) <= duration('1h')",message="failureRetryInterval must be between 5s and 1h"
	FailureRetryInterval metav1.Duration `json:"failureRetryInterval,omitempty"`

	// +kubebuilder:default="10s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('10m')",message="connectTimeout must be between 1s and 10m"
	ConnectTimeout metav1.Duration `json:"connectTimeout,omitempty"`

	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	ServiceAccountName string                        `json:"serviceAccountName,omitempty"`
	ImagePullSecrets   []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	NodeSelector       map[string]string             `json:"nodeSelector,omitempty"`
	Tolerations        []corev1.Toleration           `json:"tolerations,omitempty"`
	Affinity           *corev1.Affinity              `json:"affinity,omitempty"`
	RuntimeClassName   *string                       `json:"runtimeClassName,omitempty"`
	PriorityClassName  string                        `json:"priorityClassName,omitempty"`
}

// PtahSchemaStatus records only credential-free reconciliation evidence.
type PtahSchemaStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	Phase              ReconciliationPhase `json:"phase,omitempty"`

	// ExecutionBinding is the durable identity of the controller/runtime epoch
	// authorized to produce new reconciliation evidence. Retained evidence stays
	// historical until refreshed. Epoch changes on every component transition,
	// including a rollback to identical values.
	ExecutionBinding *ExecutionBindingStatus `json:"executionBinding,omitempty"`

	Source  SchemaSourceStatus `json:"source,omitempty"`
	Target  TargetStatus       `json:"target,omitempty"`
	Plan    *CurrentPlanStatus `json:"plan,omitempty"`
	Applied *AppliedStatus     `json:"applied,omitempty"`

	// PendingObservation is durable proof work created after an Apply Job may
	// have mutated the database. It is independent of Phase so retries and
	// suspension cannot accidentally permit another mutation first.
	PendingObservation *PendingObservationStatus `json:"pendingObservation,omitempty"`
	// PendingLockRelease keeps the exact Lease owner and epoch durable until an
	// idempotent release succeeds. It closes the manager-crash window between a
	// terminal status transition and clearing the owner-neutral Lease.
	PendingLockRelease *TargetLockReleaseStatus `json:"pendingLockRelease,omitempty"`

	ActiveOperation *ActiveOperationStatus `json:"activeOperation,omitempty"`

	LastAttemptTime              *metav1.Time `json:"lastAttemptTime,omitempty"`
	LastSuccessfulReconciliation *metav1.Time `json:"lastSuccessfulReconciliation,omitempty"`
	// NextReconciliationTime is the durable earliest time for the next
	// scheduled read-only reconciliation. Event-driven safety work may run
	// sooner.
	NextReconciliationTime *metav1.Time `json:"nextReconciliationTime,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ExecutionBindingStatus identifies one uninterrupted controller/runtime
// evidence epoch. The explicit component fields are audit evidence; Epoch
// prevents approvals from becoming current again after a later rollback.
type ExecutionBindingStatus struct {
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	Epoch string `json:"epoch"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PtahVersion string `json:"ptahVersion"`
	// +kubebuilder:validation:MinLength=1
	ExecutorImage string `json:"executorImage"`
	// +kubebuilder:validation:MinLength=1
	RunnerImage string `json:"runnerImage"`
	// +kubebuilder:validation:Minimum=1
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`
}

// TargetLockReleaseStatus is the complete credential-free request required to
// retry one exact database-realm Lease release after a manager restart.
type TargetLockReleaseStatus struct {
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CoordinationDigest string `json:"coordinationDigest"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	OperationID string `json:"operationID"`
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=86460
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds"`
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	LeaseEpoch string `json:"leaseEpoch"`
}

// AdmissionObjectBinding identifies one API object whose credential-free
// contents contributed to the resolved Pod admission envelope.
type AdmissionObjectBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
	// ResourceVersion is opaque, but bounded here so hostile metadata cannot
	// make the status object grow without limit.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ResourceVersion string `json:"resourceVersion"`
}

// LimitRangeAdmissionSnapshot contains only the container defaults that can
// legitimately fill resource keys absent from the submitted Job template.
type LimitRangeAdmissionSnapshot struct {
	Object AdmissionObjectBinding `json:"object"`
	// +kubebuilder:validation:MaxProperties=64
	DefaultRequests map[corev1.ResourceName]resource.Quantity `json:"defaultRequests,omitempty"`
	// +kubebuilder:validation:MaxProperties=64
	DefaultLimits map[corev1.ResourceName]resource.Quantity `json:"defaultLimits,omitempty"`
}

// RuntimeClassAdmissionSnapshot records the exact scheduling and overhead
// mutation selected before dispatch. Handler is deliberately retained as
// credential-free audit evidence even though it is not copied into PodSpec.
type RuntimeClassAdmissionSnapshot struct {
	Object AdmissionObjectBinding `json:"object"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Handler string `json:"handler"`
	// OverheadDefined distinguishes an absent RuntimeClass overhead stanza from
	// a present but empty one; Kubernetes admission preserves that distinction.
	OverheadDefined bool `json:"overheadDefined,omitempty"`
	// +kubebuilder:validation:MaxProperties=64
	Overhead map[corev1.ResourceName]resource.Quantity `json:"overhead,omitempty"`
	// +kubebuilder:validation:MaxProperties=64
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// PriorityClassAdmissionSnapshot records the exact values injected by the
// Priority admission plugin. Object is absent only when the cluster has no
// global default and the Job does not request a named PriorityClass.
type PriorityClassAdmissionSnapshot struct {
	Object *AdmissionObjectBinding `json:"object,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	Name  string `json:"name,omitempty"`
	Value int32  `json:"value"`
	// +kubebuilder:validation:Enum=Never;PreemptLowerPriority
	PreemptionPolicy *corev1.PreemptionPolicy `json:"preemptionPolicy,omitempty"`
}

// ServiceAccountAdmissionSnapshot binds the non-secret ServiceAccount fields
// that built-in admission may copy into a Pod.
type ServiceAccountAdmissionSnapshot struct {
	Object AdmissionObjectBinding `json:"object"`
	// +kubebuilder:validation:MaxItems=64
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// PodAdmissionSnapshot is the bounded, credential-free mutation envelope for
// one active operation. Digest covers every other field using canonical JSON.
// A controller restart validates this persisted snapshot instead of rereading
// resources whose later mutations must not reinterpret the operation.
type PodAdmissionSnapshot struct {
	// +kubebuilder:validation:Enum=v1
	Version string `json:"version"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// TemplateDigest binds the canonical, API-defaulted pre-admission Job Pod
	// template. The self-referential snapshot annotation and four exact
	// API-server-generated Job identity labels are omitted and validated
	// separately against the current Job name and UID.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	TemplateDigest string `json:"templateDigest"`

	ServiceAccount ServiceAccountAdmissionSnapshot `json:"serviceAccount"`
	// +kubebuilder:validation:MaxItems=32
	LimitRanges   []LimitRangeAdmissionSnapshot  `json:"limitRanges,omitempty"`
	RuntimeClass  *RuntimeClassAdmissionSnapshot `json:"runtimeClass,omitempty"`
	PriorityClass PriorityClassAdmissionSnapshot `json:"priorityClass"`
	// DefaultTolerationsEnabled records whether kube-apiserver runs the
	// DefaultTolerationSeconds admission plugin.
	DefaultTolerationsEnabled bool `json:"defaultTolerationsEnabled"`
	// +kubebuilder:validation:Minimum=0
	DefaultNotReadyTolerationSeconds int64 `json:"defaultNotReadyTolerationSeconds"`
	// +kubebuilder:validation:Minimum=0
	DefaultUnreachableTolerationSeconds int64 `json:"defaultUnreachableTolerationSeconds"`
	// ExtendedResourceTolerationEnabled records whether kube-apiserver runs the
	// ExtendedResourceToleration admission plugin.
	ExtendedResourceTolerationEnabled bool `json:"extendedResourceTolerationEnabled"`
	// AlwaysPullImagesEnabled records whether kube-apiserver runs the
	// AlwaysPullImages admission plugin.
	AlwaysPullImagesEnabled bool `json:"alwaysPullImagesEnabled"`
}

// SchemaSourceStatus binds the requested reference to verified immutable data.
type SchemaSourceStatus struct {
	RequestedReference string `json:"requestedReference,omitempty"`
	ResolvedReference  string `json:"resolvedReference,omitempty"`
	Digest             string `json:"digest,omitempty"`
	MediaType          string `json:"mediaType,omitempty"`
	ArtifactType       string `json:"artifactType,omitempty"`
	Size               int64  `json:"size,omitempty"`
	Verified           bool   `json:"verified,omitempty"`

	VerificationPolicyUID    types.UID    `json:"verificationPolicyUID,omitempty"`
	VerificationPolicyDigest string       `json:"verificationPolicyDigest,omitempty"`
	ResolvedAt               *metav1.Time `json:"resolvedAt,omitempty"`
	VerifiedAt               *metav1.Time `json:"verifiedAt,omitempty"`
}

// TargetStatus identifies the Secret value and observed schema without
// disclosing either the connection string or its credentials.
type TargetStatus struct {
	Engine               DatabaseEngine `json:"engine,omitempty"`
	CoordinationDigest   string         `json:"coordinationDigest,omitempty"`
	IdentityDigest       string         `json:"identityDigest,omitempty"`
	DriftReportDigest    string         `json:"driftReportDigest,omitempty"`
	LastObservedAt       *metav1.Time   `json:"lastObservedAt,omitempty"`
	HighestDriftSeverity string         `json:"highestDriftSeverity,omitempty"`
	DriftFindingCount    int32          `json:"driftFindingCount,omitempty"`
}

// CurrentPlanStatus is a compact reference to an immutable PtahSchemaPlan.
type CurrentPlanStatus struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`

	Fingerprint              string    `json:"fingerprint"`
	ContentDigest            string    `json:"contentDigest"`
	ArtifactDigest           string    `json:"artifactDigest"`
	CoordinationDigest       string    `json:"coordinationDigest"`
	TargetIdentityDigest     string    `json:"targetIdentityDigest"`
	ActualStateFingerprint   string    `json:"actualStateFingerprint"`
	DesiredStateFingerprint  string    `json:"desiredStateFingerprint"`
	PolicyFingerprint        string    `json:"policyFingerprint"`
	VerificationPolicyUID    types.UID `json:"verificationPolicyUID"`
	VerificationPolicyDigest string    `json:"verificationPolicyDigest"`
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID    string      `json:"executionBindingID,omitempty"`
	PtahVersion           string      `json:"ptahVersion"`
	ExecutorImage         string      `json:"executorImage"`
	RunnerImage           string      `json:"runnerImage"`
	RunnerProtocolVersion int32       `json:"runnerProtocolVersion"`
	Destructive           bool        `json:"destructive"`
	StatementCount        int32       `json:"statementCount"`
	CreatedAt             metav1.Time `json:"createdAt"`

	Approval *ConsumedApprovalStatus `json:"approval,omitempty"`
}

// ConsumedApprovalStatus records the immutable approval object and identity.
type ConsumedApprovalStatus struct {
	Name       string           `json:"name"`
	UID        types.UID        `json:"uid"`
	Approver   ApprovalIdentity `json:"approver"`
	ApprovedAt metav1.Time      `json:"approvedAt"`
}

// AppliedStatus is written only after post-apply observation proves convergence.
type AppliedStatus struct {
	ArtifactDigest       string `json:"artifactDigest"`
	PlanFingerprint      string `json:"planFingerprint"`
	CoordinationDigest   string `json:"coordinationDigest"`
	TargetIdentityDigest string `json:"targetIdentityDigest"`
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID    string      `json:"executionBindingID,omitempty"`
	PtahVersion           string      `json:"ptahVersion"`
	ExecutorImage         string      `json:"executorImage"`
	RunnerImage           string      `json:"runnerImage"`
	RunnerProtocolVersion int32       `json:"runnerProtocolVersion"`
	CompletedAt           metav1.Time `json:"completedAt"`
}

// PendingObservationOutcome records why read-only convergence proof is
// mandatory before another mutation may be considered.
// +kubebuilder:validation:Enum=ApplySucceeded;OutcomeUnknown
type PendingObservationOutcome string

const (
	PendingObservationApplySucceeded PendingObservationOutcome = "ApplySucceeded"
	PendingObservationOutcomeUnknown PendingObservationOutcome = "OutcomeUnknown"
)

// PendingObservationStatus binds post-apply proof to the immutable plan,
// target, policy, and lock holder that existed at the mutation boundary.
type PendingObservationStatus struct {
	Outcome          PendingObservationOutcome `json:"outcome"`
	ApplyOperationID string                    `json:"applyOperationID"`
	// ApplyJobName and ApplyJobUID identify the Kubernetes Job independently
	// of mutable labels so every exact-owner Pod can be tracked until the
	// immutable execution horizon has elapsed.
	// +kubebuilder:validation:MaxLength=253
	ApplyJobName string    `json:"applyJobName,omitempty"`
	ApplyJobUID  types.UID `json:"applyJobUID,omitempty"`
	// ApplyPodUIDs and ApplyPodCount preserve the terminal Pod evidence seen at
	// the mutation boundary. More than one Pod always forces outcome-unknown
	// proof even for a one-shot Job.
	// +kubebuilder:validation:MaxItems=8
	ApplyPodUIDs []types.UID `json:"applyPodUIDs,omitempty"`
	// +kubebuilder:validation:Minimum=0
	ApplyPodCount   int32 `json:"applyPodCount,omitempty"`
	ApplyGeneration int64 `json:"applyGeneration"`
	// ObserveAfter delays proof when the Kubernetes Job identity or create
	// result is uncertain. Until this time, the original mutating Pod could
	// still be within its immutable active deadline.
	ObserveAfter *metav1.Time `json:"observeAfter,omitempty"`

	Plan               CurrentPlanStatus        `json:"plan"`
	Target             DatabaseTargetBinding    `json:"target"`
	CoordinationDigest string                   `json:"coordinationDigest"`
	Source             OCIArtifactAccessBinding `json:"source"`
	Dev                *DatabaseTargetRef       `json:"dev,omitempty"`
	// PlanRequired records that the raw drift read completed and the same
	// immutable proof inputs now require authoritative managed-scope planning.
	PlanRequired bool `json:"planRequired,omitempty"`

	Exclude        []string        `json:"exclude,omitempty"`
	DriftSeverity  string          `json:"driftSeverity,omitempty"`
	ConnectTimeout metav1.Duration `json:"connectTimeout,omitempty"`
	LockTimeout    metav1.Duration `json:"lockTimeout,omitempty"`

	// LeaseDurationSeconds is the immutable duration claimed for the Apply
	// operation. The same holder remains active through convergence proof.
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=86460
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds"`
	// LeaseEpoch identifies the uninterrupted database-realm lock acquisition
	// carried from Apply through its complete read-only convergence proof.
	LeaseEpoch string `json:"leaseEpoch,omitempty"`
}

// ActiveOperationStatus makes controller restarts resume one deterministic Job.
type ActiveOperationStatus struct {
	Type             OperationType `json:"type"`
	ID               string        `json:"id"`
	InputFingerprint string        `json:"inputFingerprint"`
	JobName          string        `json:"jobName"`
	JobUID           types.UID     `json:"jobUID,omitempty"`
	StartedAt        metav1.Time   `json:"startedAt"`
	Attempt          int32         `json:"attempt"`
	// ExecutionBindingID binds every Job and result to the durable evidence
	// epoch that authorized its claim.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID,omitempty"`

	// AdmissionSnapshot is persisted before dispatch and is bound into the Job
	// and Pod template annotations. It permits only modeled, safe built-in
	// admission mutations while retaining exact validation for executable and
	// security-sensitive Pod fields.
	AdmissionSnapshot *PodAdmissionSnapshot `json:"admissionSnapshot,omitempty"`

	// DispatchStarted is persisted immediately before the one permitted Job
	// create attempt. A missing Apply Job after this boundary is outcome-unknown
	// and must never be recreated.
	DispatchStarted bool `json:"dispatchStarted,omitempty"`
	// DispatchNotAfter is the immutable last instant at which the Apply runner
	// may start its mutating child. Missing or untrusted terminal Pod evidence
	// keeps proof behind the complete execution horizon below.
	DispatchNotAfter *metav1.Time `json:"dispatchNotAfter,omitempty"`
	// ExecutionNotAfter is enforced by the runner as the mutating child context
	// deadline, independently of relative Job and Pod deadlines.
	ExecutionNotAfter *metav1.Time `json:"executionNotAfter,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// Plan and Apply operations persist their credential-free lock binding so
	// later spec changes cannot redirect or shorten protection for a running Job.
	CoordinationDigest        string                 `json:"coordinationDigest,omitempty"`
	TargetIdentityDigest      string                 `json:"targetIdentityDigest,omitempty"`
	Target                    *DatabaseTargetBinding `json:"target,omitempty"`
	ObservationExclude        []string               `json:"observationExclude,omitempty"`
	ObservationSeverity       string                 `json:"observationSeverity,omitempty"`
	ObservationDev            *DatabaseTargetRef     `json:"observationDev,omitempty"`
	ObservationConnectTimeout metav1.Duration        `json:"observationConnectTimeout,omitempty"`
	ObservationLockTimeout    metav1.Duration        `json:"observationLockTimeout,omitempty"`
	// VerificationPolicyUID and VerificationPolicyDigest bind a Verify Job to
	// the immutable ConfigMap version inspected before dispatch.
	VerificationPolicyUID    types.UID `json:"verificationPolicyUID,omitempty"`
	VerificationPolicyDigest string    `json:"verificationPolicyDigest,omitempty"`
	// Source snapshots artifact access for mandatory post-Apply observation.
	// It contains Kubernetes selectors only, never Secret contents.
	Source *OCIArtifactAccessBinding `json:"source,omitempty"`
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=86460
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds,omitempty"`
	// LeaseEpoch is persisted before dispatch. A different epoch invalidates
	// every result produced by this operation.
	LeaseEpoch string `json:"leaseEpoch,omitempty"`
	// LeaseContinuityLost is set before any result can be harvested when the
	// persisted epoch no longer owns an uninterrupted lock interval.
	LeaseContinuityLost bool `json:"leaseContinuityLost,omitempty"`
}

const (
	ConditionArtifactResolved     = "ArtifactResolved"
	ConditionArtifactVerified     = "ArtifactVerified"
	ConditionDatabaseReachable    = "DatabaseReachable"
	ConditionDriftDetected        = "DriftDetected"
	ConditionPlanReady            = "PlanReady"
	ConditionApprovalRequired     = "ApprovalRequired"
	ConditionApplying             = "Applying"
	ConditionInSync               = "InSync"
	ConditionReady                = "Ready"
	ConditionSuspended            = "Suspended"
	ConditionReconciliationFailed = "ReconciliationFailed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ptahs
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.source.digest`,priority=1
// +kubebuilder:printcolumn:name="Drift",type=string,JSONPath=`.status.conditions[?(@.type=='DriftDetected')].status`
// +kubebuilder:printcolumn:name="Approval",type=string,JSONPath=`.status.conditions[?(@.type=='ApprovalRequired')].status`
// +kubebuilder:printcolumn:name="In Sync",type=string,JSONPath=`.status.conditions[?(@.type=='InSync')].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PtahSchema struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PtahSchemaSpec   `json:"spec"`
	Status PtahSchemaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PtahSchemaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PtahSchema `json:"items"`
}

// JobConditionType is retained as an alias for consumers that need to compare
// terminal execution state without importing the batch API separately.
type JobConditionType = batchv1.JobConditionType
