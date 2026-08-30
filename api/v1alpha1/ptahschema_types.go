package v1alpha1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
)

// PtahSchemaSpec declares one desired schema and one database target.
type PtahSchemaSpec struct {
	Target  DatabaseTargetSpec    `json:"target"`
	Desired OCIArtifactSourceSpec `json:"desired"`
	Dev     *DatabaseTargetRef    `json:"dev,omitempty"`
	Policy  ReconciliationPolicy  `json:"policy,omitempty"`

	// Interval is the cadence for resolving mutable tags and observing drift.
	// +kubebuilder:default="10m"
	Interval metav1.Duration `json:"interval,omitempty"`

	Execution ExecutionSpec `json:"execution,omitempty"`

	// Suspend prevents new Jobs. A Job already applying is observed to a
	// terminal result and is never replaced by a destructive cleanup action.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// DatabaseTargetSpec identifies a supported engine and a namespaced Secret
// key. There is deliberately no namespace field.
type DatabaseTargetSpec struct {
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

// DesiredSchemaSpec is retained as a source-compatible name for clients that
// used the provisional API type before the common OCI source was factored out.
type DesiredSchemaSpec = OCIArtifactSourceSpec

// RegistryAuthSource describes a Secret without requiring the controller to
// read it. The kubelet projects only the selected representation into a Job.
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
	// +kubebuilder:default=registry
	RegistryKey string `json:"registryKey,omitempty"`

	// +kubebuilder:default=.dockerconfigjson
	DockerConfigJSONKey string `json:"dockerConfigJSONKey,omitempty"`
}

// OCITransportSpec configures private and air-gapped registries without
// allowing arbitrary files or commands into the execution Pod.
type OCITransportSpec struct {
	// PlainHTTP is intended only for explicitly trusted test or air-gapped
	// networks. HTTPS remains the default.
	// +kubebuilder:default=false
	PlainHTTP bool `json:"plainHTTP,omitempty"`

	CAFrom *corev1.ConfigMapKeySelector `json:"caFrom,omitempty"`

	ClientCertificateFrom *TLSSecretReference `json:"clientCertificateFrom,omitempty"`
}

// TLSSecretReference selects a complete client certificate credential.
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

	// Ignore contains drift selectors understood by the Ptah CLI.
	// +kubebuilder:validation:MaxItems=128
	Ignore []string `json:"ignore,omitempty"`

	// Exclude contains planning selectors. Keeping it distinct from Ignore
	// avoids silently translating between selector languages.
	// +kubebuilder:validation:MaxItems=128
	Exclude []string `json:"exclude,omitempty"`

	// +kubebuilder:default="30s"
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
	FailureRetryInterval metav1.Duration `json:"failureRetryInterval,omitempty"`

	// +kubebuilder:default="10s"
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

	Source  SchemaSourceStatus `json:"source,omitempty"`
	Target  TargetStatus       `json:"target,omitempty"`
	Plan    *CurrentPlanStatus `json:"plan,omitempty"`
	Applied *AppliedStatus     `json:"applied,omitempty"`

	ActiveOperation *ActiveOperationStatus `json:"activeOperation,omitempty"`

	LastAttemptTime              *metav1.Time `json:"lastAttemptTime,omitempty"`
	LastSuccessfulReconciliation *metav1.Time `json:"lastSuccessfulReconciliation,omitempty"`
	NextReconciliationTime       *metav1.Time `json:"nextReconciliationTime,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
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

	VerificationPolicyDigest string       `json:"verificationPolicyDigest,omitempty"`
	ResolvedAt               *metav1.Time `json:"resolvedAt,omitempty"`
	VerifiedAt               *metav1.Time `json:"verifiedAt,omitempty"`
}

// TargetStatus identifies the Secret value and observed schema without
// disclosing either the connection string or its credentials.
type TargetStatus struct {
	Engine                   DatabaseEngine `json:"engine,omitempty"`
	IdentityDigest           string         `json:"identityDigest,omitempty"`
	ObservedStateFingerprint string         `json:"observedStateFingerprint,omitempty"`
	LastObservedAt           *metav1.Time   `json:"lastObservedAt,omitempty"`
	HighestDriftSeverity     string         `json:"highestDriftSeverity,omitempty"`
	DriftFindingCount        int32          `json:"driftFindingCount,omitempty"`
}

// CurrentPlanStatus is a compact reference to an immutable PtahSchemaPlan.
type CurrentPlanStatus struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`

	Fingerprint              string      `json:"fingerprint"`
	ContentDigest            string      `json:"contentDigest"`
	ArtifactDigest           string      `json:"artifactDigest"`
	TargetIdentityDigest     string      `json:"targetIdentityDigest"`
	ActualStateFingerprint   string      `json:"actualStateFingerprint"`
	DesiredStateFingerprint  string      `json:"desiredStateFingerprint"`
	PolicyFingerprint        string      `json:"policyFingerprint"`
	VerificationPolicyDigest string      `json:"verificationPolicyDigest"`
	PtahVersion              string      `json:"ptahVersion"`
	ExecutorImage            string      `json:"executorImage"`
	RunnerImage              string      `json:"runnerImage"`
	RunnerProtocolVersion    int32       `json:"runnerProtocolVersion"`
	Destructive              bool        `json:"destructive"`
	StatementCount           int32       `json:"statementCount"`
	CreatedAt                metav1.Time `json:"createdAt"`

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
	ArtifactDigest        string      `json:"artifactDigest"`
	PlanFingerprint       string      `json:"planFingerprint"`
	TargetIdentityDigest  string      `json:"targetIdentityDigest"`
	PtahVersion           string      `json:"ptahVersion"`
	ExecutorImage         string      `json:"executorImage"`
	RunnerImage           string      `json:"runnerImage"`
	RunnerProtocolVersion int32       `json:"runnerProtocolVersion"`
	CompletedAt           metav1.Time `json:"completedAt"`
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

	Spec   PtahSchemaSpec   `json:"spec,omitempty"`
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
