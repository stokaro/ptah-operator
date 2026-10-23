package v1alpha1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DatabaseEngine names a database family. The API accepts bounded engine names
// so the controller can report unsupported families through status instead of
// turning a durable desired-state object into an admission-time dead end.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9._-]*$`
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

	// The fixed keys of a Git credential Secret. GitHostSecretKey is the
	// authority, host[:port], the credential owner grants the credential
	// for; GitCASHA256SecretKey and GitKnownHostsSHA256SecretKey are the
	// "sha256:" digests of the trust it may be sent under. A resource's
	// author selects the Secret, never the keys.
	GitUsernameSecretKey         = "username"
	GitPasswordSecretKey         = "password"
	GitSSHPrivateKeySecretKey    = "sshPrivateKey"
	GitHostSecretKey             = "host"
	GitCASHA256SecretKey         = "caSHA256"
	GitKnownHostsSHA256SecretKey = "knownHostsSHA256"
)

// PtahSchemaSpec declares one desired schema and one database target.
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.connectTimeout) || !has(self.execution.activeDeadlineSeconds) || duration(self.execution.connectTimeout).getMilliseconds() <= self.execution.activeDeadlineSeconds * 1000",message="execution.connectTimeout must not exceed execution.activeDeadlineSeconds"
// +kubebuilder:validation:XValidation:rule="!has(self.policy) || !has(self.policy.lockTimeout) || !has(self.execution) || !has(self.execution.activeDeadlineSeconds) || duration(self.policy.lockTimeout).getMilliseconds() <= self.execution.activeDeadlineSeconds * 1000",message="policy.lockTimeout must not exceed execution.activeDeadlineSeconds"
// +kubebuilder:validation:XValidation:rule="self.target.urlFrom.name.size() > 0 && self.target.urlFrom.key.size() > 0 && (!has(self.target.urlFrom.optional) || !self.target.urlFrom.optional)",message="target.urlFrom must name a required Secret key"
// +kubebuilder:validation:XValidation:rule="!has(self.desired.verificationPolicyFrom) || (self.desired.verificationPolicyFrom.name.size() > 0 && self.desired.verificationPolicyFrom.key.size() > 0 && (!has(self.desired.verificationPolicyFrom.optional) || !self.desired.verificationPolicyFrom.optional))",message="desired.verificationPolicyFrom must name a required ConfigMap key"
// +kubebuilder:validation:XValidation:rule="!has(self.desired.configMap) || self.desired.configMap.items.size() == 1",message="desired.configMap selects exactly one file for a schema"
// +kubebuilder:validation:XValidation:rule="!has(self.desired.git) || self.desired.git.path != '.'",message="desired.git.path selects one file for a schema"
// +kubebuilder:validation:XValidation:rule="!has(self.dev) || (self.dev.urlFrom.name.size() > 0 && self.dev.urlFrom.key.size() > 0 && (!has(self.dev.urlFrom.optional) || !self.dev.urlFrom.optional))",message="dev.urlFrom must name a required Secret key"
// +kubebuilder:validation:XValidation:rule="!has(self.desired.transport) || !has(self.desired.transport.caFrom) || (self.desired.transport.caFrom.name.size() > 0 && self.desired.transport.caFrom.key.size() > 0 && (!has(self.desired.transport.caFrom.optional) || !self.desired.transport.caFrom.optional))",message="desired.transport.caFrom must name a required ConfigMap key"
type PtahSchemaSpec struct {
	// Target is the database to converge, named through a Secret the manager
	// itself has no permission to read.
	Target DatabaseTargetSpec `json:"target"`
	// Desired is the source that declares the schema, and the rows a
	// declaration names, to converge it to: an OCI artifact or a ConfigMap.
	Desired SourceSpec `json:"desired"`
	// Dev is a scratch database Ptah may use where a comparison needs one. It
	// is never the target, and nothing it holds is kept.
	Dev *DatabaseTargetRef `json:"dev,omitempty"`
	// Policy decides what may happen without a person: whether a plan applies
	// itself, whether a destructive one is permitted at all, what counts as
	// drift, and which tables are fenced off entirely.
	// +kubebuilder:default={}
	Policy ReconciliationPolicy `json:"policy,omitempty"`

	// Interval is the cadence for resolving mutable tags and observing drift.
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s') && duration(self) <= duration('24h')",message="interval must be between 10s and 24h"
	Interval metav1.Duration `json:"interval,omitempty"`

	// Default the object itself so the API server also applies the nested
	// execution defaults when a manifest omits the whole block.
	// +kubebuilder:default={}
	Execution ExecutionSpec `json:"execution,omitempty"`

	// Suspend prevents new Jobs. A Job already applying is observed to a
	// terminal result and is never replaced by a destructive cleanup action.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// DatabaseTargetSpec identifies a supported engine, a namespaced Secret key,
// and the coordination realm of the physical database: a key this namespace
// owns, or a PtahRealm an administrator admitted this namespace to. There is
// deliberately no namespace field.
// +kubebuilder:validation:XValidation:rule="has(self.coordinationKey) != has(self.realmRef)",message="set exactly one of coordinationKey and realmRef"
type DatabaseTargetSpec struct {
	// Engine is the database this target speaks. An engine outside the
	// supported set is refused with a condition rather than attempted.
	Engine DatabaseEngine `json:"engine"`

	// CoordinationKey is a non-secret, stable identifier for the physical
	// database, scoped to this resource's namespace. Every resource in the
	// namespace that can reach the same database through an alias, proxy, or
	// different credential must use exactly the same key.
	//
	// The same key in another namespace is another realm: a resource elsewhere
	// can neither block this one nor take turns with it by choosing the same
	// string. A database more than one namespace manages is named with
	// realmRef instead, which an administrator has to grant.
	//
	// Exactly one of coordinationKey and realmRef is set.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9](?:[a-z0-9._:/-]{0,251}[a-z0-9])?$`
	CoordinationKey string `json:"coordinationKey,omitempty"`

	// RealmRef names the cluster-scoped PtahRealm this database belongs to.
	// The realm, not this resource, decides whether the claim is allowed: a
	// resource whose namespace the realm does not list, or whose engine it
	// does not name, is refused with reason RealmNotAuthorized and runs
	// nothing, and it is not counted against the resources the realm does
	// admit.
	//
	// Exactly one of coordinationKey and realmRef is set.
	// +optional
	RealmRef *PtahRealmReference `json:"realmRef,omitempty"`

	// SharedRealm declares that this resource manages only part of the database
	// its realm names, and that every other resource managing that database
	// has declared the same.
	//
	// It defaults to false. A realm more than one resource claims is refused
	// while any claimant leaves it false -- including the resource that did
	// declare it. A PtahRealm with sharing Exclusive refuses a second claimant
	// whatever this says.
	//
	// Deleting a resource leaves the realm, and so does suspending it;
	// resuming puts it back, and the conflict is refused then, before any Job.
	//
	// What is verified is the declaration, never the disjointness: nothing can
	// tell whether two sets of arbitrary SQL touch the same rows. The
	// concurrency section of the architecture reference says why taking turns
	// is not enough.
	// +kubebuilder:default=false
	SharedRealm bool `json:"sharedRealm,omitempty"`

	// URLFrom names the Secret key holding the connection URL. The manager has
	// no permission to read it: the operation Pod resolves it, and the URL
	// never reaches status, an Event or a command line.
	URLFrom corev1.SecretKeySelector `json:"urlFrom"`
}

// DatabaseTargetBinding is the key-free immutable target snapshot stored in
// status. CoordinationKey must never be copied into status; its digest is
// persisted separately.
type DatabaseTargetBinding struct {
	// Engine is the database this binding speaks.
	Engine DatabaseEngine `json:"engine"`
	// URLFrom is the Secret key the operation Pod reads the URL from.
	URLFrom corev1.SecretKeySelector `json:"urlFrom"`
}

// DatabaseTargetRef is a database URL reference used for optional rehearsal.
type DatabaseTargetRef struct {
	// URLFrom names the Secret key holding this database's connection URL.
	URLFrom corev1.SecretKeySelector `json:"urlFrom"`
}

// SourceSpec names where the desired files come from: an OCI artifact, a
// ConfigMap in the resource's own namespace, or one commit of a Git
// repository. The schema and the migration APIs share it, and keep their
// reconciliation lifecycles separate.
// +kubebuilder:validation:XValidation:rule="[has(self.ociRef), has(self.configMap), has(self.git)].filter(set, set).size() == 1",message="set exactly one of ociRef, configMap and git"
// +kubebuilder:validation:XValidation:rule="has(self.ociRef) == has(self.verificationPolicyFrom)",message="verificationPolicyFrom is required with ociRef and refused with configMap and git"
// +kubebuilder:validation:XValidation:rule="has(self.ociRef) || !has(self.registryAuthFrom)",message="registryAuthFrom is refused with configMap and git"
// +kubebuilder:validation:XValidation:rule="has(self.ociRef) || !has(self.transport) || (!has(self.transport.caFrom) && (!has(self.transport.plainHTTP) || !self.transport.plainHTTP))",message="transport is refused with configMap and git"
// +kubebuilder:validation:XValidation:rule="!has(self.sourcePolicyFrom) || !has(self.ociRef)",message="sourcePolicyFrom is for configMap and git sources"
// +kubebuilder:validation:XValidation:rule="!has(self.sourcePolicyFrom) || (self.sourcePolicyFrom.name.size() > 0 && self.sourcePolicyFrom.key.size() > 0 && (!has(self.sourcePolicyFrom.optional) || !self.sourcePolicyFrom.optional))",message="sourcePolicyFrom must name a required ConfigMap key"
type SourceSpec struct {
	// OCIRef is a desired-schema artifact reference. It may name a tag or a
	// digest; every later operation receives only the resolved digest.
	// +optional
	// +kubebuilder:validation:MinLength=7
	// +kubebuilder:validation:Pattern=`^oci://[^[:space:]?#]+$`
	OCIRef string `json:"ociRef,omitempty"`

	// ConfigMap selects files from a ConfigMap in this resource's namespace.
	// Whoever may write that ConfigMap may change the desired state, exactly
	// as whoever may write this resource can: Kubernetes RBAC on the
	// namespace is what this source is trusted by. The manager copies the
	// selected files into an immutable snapshot, and every operation reads
	// the snapshot.
	// +optional
	ConfigMap *ConfigMapSourceSpec `json:"configMap,omitempty"`

	// Git selects a file or a directory of one commit of a Git repository.
	// The manager never reads the repository or its credential: a Job's fetch
	// container fetches the commit through Ptah, and every later operation
	// fetches the same commit again and refuses any other content.
	// +optional
	Git *GitSourceSpec `json:"git,omitempty"`

	// RegistryAuthFrom names the Secret an operation Pod reads the registry
	// credential from. The process that runs SQL never receives it.
	RegistryAuthFrom *RegistryAuthSource `json:"registryAuthFrom,omitempty"`

	// VerificationPolicyFrom names the immutable ConfigMap holding the policy
	// the artifact must satisfy. Editing it retires the plans computed under
	// the previous version rather than letting them apply. It is required
	// with ociRef and refused with configMap: its rules are about OCI
	// artifacts, and a policy nothing evaluates would read as a gate.
	// +optional
	VerificationPolicyFrom *corev1.ConfigMapKeySelector `json:"verificationPolicyFrom,omitempty"`

	// SourcePolicyFrom names the immutable ConfigMap holding what a team
	// refuses for a ConfigMap or Git source: the hosts a Git source may
	// name, the ConfigMaps a ConfigMap source may name, and the most the
	// selected files may hold. A plan binds the policy's object and bytes,
	// so another policy retires it. An OCI source has its verification
	// policy instead, and the API refuses this beside ociRef.
	// +optional
	SourcePolicyFrom *corev1.ConfigMapKeySelector `json:"sourcePolicyFrom,omitempty"`

	// Transport is how the registry is reached: plain HTTP, a custom CA.
	Transport OCITransportSpec `json:"transport,omitempty"`
}

// ConfigMapSourceSpec selects files from one ConfigMap.
type ConfigMapSourceSpec struct {
	// Name of the ConfigMap, in this resource's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Items are the keys that become files, and the file each one becomes. A
	// key cannot hold "/", so a nested directory is spelled out here. A key
	// the list does not name is not part of the source, and editing it
	// changes nothing. A schema selects exactly one file, whose extension
	// tells Ptah its format; a migration directory selects up to 1,000.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	// +listType=map
	// +listMapKey=path
	Items []ConfigMapSourceItem `json:"items"`
}

// ConfigMapSourceItem maps one ConfigMap key to one file.
type ConfigMapSourceItem struct {
	// Key in the ConfigMap's data or binaryData.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`

	// Path is the file the key becomes: relative, separated by "/", with no
	// empty, "." or ".." element, no backslash and no control character. An
	// element longer than 255 bytes, or a path that is a file and a directory
	// at once, is refused when the source is read.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+)(?:/(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+))*$`
	Path string `json:"path"`
}

// GitSourceSpec selects a file or a directory of a Git repository.
// +kubebuilder:validation:XValidation:rule="!self.url.startsWith('ssh://') || (has(self.transport) && has(self.transport.knownHostsFrom))",message="an ssh:// repository needs transport.knownHostsFrom"
// +kubebuilder:validation:XValidation:rule="self.url.startsWith('ssh://') || !has(self.transport) || !has(self.transport.knownHostsFrom)",message="transport.knownHostsFrom is for an ssh:// repository"
// +kubebuilder:validation:XValidation:rule="!self.url.startsWith('ssh://') || !has(self.transport) || !has(self.transport.caFrom)",message="transport.caFrom is for an https:// repository"
type GitSourceSpec struct {
	// URL of the repository: https:// or ssh://, with no credential, query or
	// fragment.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^(https://[^@/[:space:]?#]+|ssh://([^:@/[:space:]?#]+@)?[^@/[:space:]?#]+)/[^[:space:]?#]+$`
	URL string `json:"url"`

	// Ref says which commit: a branch or a tag, polled at the resource's
	// interval, or a commit.
	Ref GitRef `json:"ref"`

	// Path is what the commit's tree selects: the schema file, or the
	// migration directory, where "." is the root. Relative, separated by
	// "/", with no empty or ".." element, no backslash and no control
	// character.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^(\.|(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+)(?:/(?:[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.[^./\\\x00-\x1f\x7f][^/\\\x00-\x1f\x7f]*|\.\.[^/\\\x00-\x1f\x7f]+))*)$`
	Path string `json:"path"`

	// AuthFrom names the Secret the fetch container reads the credential
	// from. The manager never reads it.
	// +optional
	AuthFrom *GitAuthSource `json:"authFrom,omitempty"`

	// Transport pins whom the fetch talks to: the CA an https:// server's
	// certificate chains to, or the host keys an ssh:// server presents.
	// +optional
	Transport GitTransportSpec `json:"transport,omitempty"`
}

// GitRef names one commit: exactly one of a branch, a tag and a commit id.
// +kubebuilder:validation:XValidation:rule="[has(self.branch), has(self.tag), has(self.commit)].filter(set, set).size() == 1",message="set exactly one of branch, tag and commit"
type GitRef struct {
	// Branch is read at its tip, again at every interval.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]~^:?*\[\\]+$`
	Branch string `json:"branch,omitempty"`
	// Tag is read at the commit it names, again at every interval.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]~^:?*\[\\]+$`
	Tag string `json:"tag,omitempty"`
	// Commit is one commit, by its full 40-character id, which Ptah asks the
	// server for by that id. Unless a branch or tag points at it, Ptah asks
	// only a server that says it gives commits by id; on any other, Resolve
	// refuses the commit once its branch moves past it. A tag holds one
	// commit on every server.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit,omitempty"`
}

// GitAuthSource names the Secret a Git fetch authenticates with. Its keys are
// fixed: username and password for https:// (a token goes in password),
// sshPrivateKey for ssh://, and host, the authority host[:port] the
// credential is for, which the fetch holds the URL to. caSHA256 or
// knownHostsSHA256, when present, is the SHA-256 of the trust the transport
// selects: the Secret's owner, not the resource's author, decides whom the
// credential is sent to.
type GitAuthSource struct {
	// Name of the Secret, in this resource's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// GitTransportSpec pins the server a Git fetch talks to.
// +kubebuilder:validation:XValidation:rule="!has(self.caFrom) || (self.caFrom.name.size() > 0 && self.caFrom.key.size() > 0 && (!has(self.caFrom.optional) || !self.caFrom.optional))",message="caFrom must name a required ConfigMap key"
// +kubebuilder:validation:XValidation:rule="!has(self.knownHostsFrom) || (self.knownHostsFrom.name.size() > 0 && self.knownHostsFrom.key.size() > 0 && (!has(self.knownHostsFrom.optional) || !self.knownHostsFrom.optional))",message="knownHostsFrom must name a required ConfigMap key"
type GitTransportSpec struct {
	// CAFrom selects the only roots an https:// server's certificate may
	// chain to. With authFrom, its Secret must hold caSHA256 with the
	// selected bytes' digest.
	// +optional
	CAFrom *corev1.ConfigMapKeySelector `json:"caFrom,omitempty"`
	// KnownHostsFrom selects the host keys an ssh:// server may present; a
	// fetch never trusts a key on first use. With authFrom, its Secret must
	// hold knownHostsSHA256 with the selected bytes' digest.
	// +optional
	KnownHostsFrom *corev1.ConfigMapKeySelector `json:"knownHostsFrom,omitempty"`
}

// SourceAccessBinding is the credential-free, immutable account of how an
// operation reads the desired files: an OCI artifact pinned by digest with the
// Kubernetes selectors needed to fetch it, or the snapshot a ConfigMap source
// was copied into. It is persisted across post-Apply proof so a newer
// generation cannot send newly selected credentials to the old artifact's
// registry, or read files the plan was not computed from.
// +kubebuilder:validation:XValidation:rule="[has(self.resolvedReference), has(self.snapshot), has(self.git)].filter(set, set).size() == 1",message="set exactly one of resolvedReference, snapshot and git"
type SourceAccessBinding struct {
	// ResolvedReference is the artifact with its tag replaced by a digest.
	// +optional
	ResolvedReference string `json:"resolvedReference,omitempty"`
	// Digest is that digest on its own.
	// +optional
	Digest string `json:"digest,omitempty"`
	// RegistryAuthFrom names the Secret an operation Pod reads the registry
	// credential from. It is a selector, never the credential.
	RegistryAuthFrom *RegistryAuthSource `json:"registryAuthFrom,omitempty"`
	// Transport is how the registry is reached: plain HTTP, a custom CA.
	Transport OCITransportSpec `json:"transport,omitempty"`
	// Snapshot is the immutable ConfigMap a ConfigMap source's files were
	// copied into, and the content it must hold.
	// +optional
	Snapshot *SourceSnapshotBinding `json:"snapshot,omitempty"`
	// Git is the commit a Git source resolved to, where to fetch it from,
	// and the content it must give.
	// +optional
	Git *GitSourceBinding `json:"git,omitempty"`
}

// GitSourceBinding is what an operation fetches a Git source by: the
// repository, the ref that named the commit, the commit, the selected path,
// and the selectors of the credential and the trust. The operation fetches
// that commit again and refuses content other than ContentDigest.
type GitSourceBinding struct {
	// URL of the repository.
	URL string `json:"url"`
	// Ref is the branch or tag that named the commit when it resolved, or
	// the commit itself. A fetch reads the commit by the ref while the ref
	// still points at it.
	Ref GitRef `json:"ref"`
	// Commit is the full id of the commit.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`
	// Path is what the commit's tree selects.
	Path string `json:"path"`
	// ContentDigest is the content digest of the selected files.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
	// AuthFrom names the credential Secret. It is a selector, never the
	// credential.
	// +optional
	AuthFrom *GitAuthSource `json:"authFrom,omitempty"`
	// Transport pins the server.
	// +optional
	Transport GitTransportSpec `json:"transport,omitempty"`
}

// SourceSnapshotBinding names the snapshot an operation reads a ConfigMap
// source through, and the content it has to hold. The operation Pod checks
// every file and the manifest against the content digest before the
// operation starts, and refuses a snapshot that no longer gives it.
type SourceSnapshotBinding struct {
	// Name of the snapshot ConfigMap, in the resource's namespace.
	// +kubebuilder:validation:Pattern=`^ptah-src-[0-9a-f]{24}$`
	Name string `json:"name"`
	// UID the snapshot had when it was bound. A snapshot deleted and written
	// again under the same name is another object.
	UID types.UID `json:"uid"`
	// ContentDigest is the content digest of the selected files: the SHA-256
	// of a manifest with one line per file, sorted by path, naming the
	// file's SHA-256, its size and its path.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
	// SchemaFile is the path, inside the snapshot, of the one file a schema
	// reads. A migration reads the whole directory and leaves it empty.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	SchemaFile string `json:"schemaFile,omitempty"`
}

// GitSourceRevision is the commit a Git source last resolved to, and what
// was asked for: another repository, ref or path in the spec is another
// source, and is resolved before anything reads it.
type GitSourceRevision struct {
	// URL of the repository.
	URL string `json:"url"`
	// Ref is the branch, tag or commit that was resolved.
	Ref GitRef `json:"ref"`
	// Path is what the commit's tree selects.
	Path string `json:"path"`
	// Commit is the full id of the commit the ref named.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Commit string `json:"commit"`
	// ContentDigest is the content digest of the selected files at that
	// commit.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
}

// ConfigMapSourceRevision is the ConfigMap a source was last read from.
type ConfigMapSourceRevision struct {
	// Name of the ConfigMap.
	Name string `json:"name"`
	// UID of the ConfigMap. Another ConfigMap under the same name is another
	// source, and retires a plan computed from this one.
	UID types.UID `json:"uid"`
	// ResourceVersion is the version that was read. An edit to a key the
	// source does not select moves it and changes no content.
	ResourceVersion string `json:"resourceVersion"`
}

// SourceKind names a kind of source a plan can be bound to besides an OCI
// artifact, which plans bind through artifactDigest.
// +kubebuilder:validation:Enum=ConfigMap;Git
type SourceKind string

const (
	// SourceKindConfigMap is a ConfigMap in the resource's namespace.
	SourceKindConfigMap SourceKind = "ConfigMap"
	// SourceKindGit is one commit of a Git repository.
	SourceKindGit SourceKind = "Git"
)

// PlanSourceBinding is what a plan computed from a source other than an OCI
// artifact binds in place of artifactDigest.
type PlanSourceBinding struct {
	// Kind of source the plan was computed from.
	Kind SourceKind `json:"kind"`
	// Revision is the source object the files were read from. For a ConfigMap
	// it is the ConfigMap's UID: an edit to a key the source does not select
	// keeps the plan, while another ConfigMap, or the same name deleted and
	// created again, retires it even when the files match. For Git it is
	// "sha256:" and the SHA-256 of the repository URL and the selected path,
	// joined by a newline: another repository or another path retires the
	// plan even when the files match, while a commit that leaves the
	// selected files as they were keeps it, as an edit to an unselected key
	// keeps a ConfigMap's.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Revision string `json:"revision"`
	// ContentDigest is the content digest of the files the plan was computed
	// from.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
	// Policy is the source policy the source was checked against, when the
	// resource names one.
	// +optional
	Policy *SourcePolicyBinding `json:"policy,omitempty"`
}

// SourcePolicyBinding is the source policy a resolution was checked against:
// the ConfigMap's UID and the digest of the selected bytes.
type SourcePolicyBinding struct {
	// UID of the policy ConfigMap. Another ConfigMap under the same name is
	// another policy.
	UID types.UID `json:"uid"`
	// Digest is "sha256:" and the SHA-256 of the policy document.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// RegistryAuthSource describes a Secret without requiring the controller to
// read it. The kubelet projects only the selected credential representation
// into a Job, while every mode also projects the fixed registry authority grant
// to the runner. That grant is the Secret's `registry` key, holding the
// authority-only host[:port] the credential is for. The key is fixed so the
// Secret owner, rather than a resource author, controls the grant.
type RegistryAuthSource struct {
	// Name of the Secret the registry credential is read from. The manager
	// never reads it; the operation Pod does.
	Name string `json:"name"`

	// +kubebuilder:default=Environment
	// Mode says how the credential reaches the executor: as environment
	// variables, or as a Docker config file.
	Mode RegistryAuthMode `json:"mode,omitempty"`

	// Environment mode supports username/password or an identity token. Keys
	// are optional so a single Secret shape can use either credential form.
	// +kubebuilder:default=username
	// UsernameKey is the Secret key holding the username.
	UsernameKey string `json:"usernameKey,omitempty"`
	// +kubebuilder:default=password
	// PasswordKey is the Secret key holding the password.
	PasswordKey string `json:"passwordKey,omitempty"`
	// +kubebuilder:default=token
	// TokenKey is the Secret key holding a bearer token, where one is used
	// instead of a username and password.
	TokenKey string `json:"tokenKey,omitempty"`

	// +kubebuilder:default=.dockerconfigjson
	// DockerConfigJSONKey is the Secret key holding a Docker config document.
	DockerConfigJSONKey string `json:"dockerConfigJSONKey,omitempty"`
}

// OCITransportSpec configures private and air-gapped registries without
// allowing arbitrary files or commands into the execution Pod.
// +kubebuilder:validation:XValidation:rule="!self.plainHTTP || !has(self.caFrom)",message="caFrom cannot be used with plainHTTP"
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
}

// ReconciliationPolicy defines safety decisions. Destructive plans always
// need both allowDestructive=true and a matching approval, even in Always mode,
// and a plan that changes privileges needs a matching approval in every mode.
type ReconciliationPolicy struct {
	// Apply decides when a current plan may run: never, only with an approval
	// naming its exact bytes, or as soon as it is ready. Always still waits for
	// an approval when the plan is destructive or changes privileges: grants,
	// role membership, owners, row-security policies or definer rights.
	// +kubebuilder:default=OnApproval
	Apply ApplyPolicy `json:"apply,omitempty"`

	// AllowDestructive permits a plan that drops or rewrites something. It is
	// permission for the category, not for a plan: a destructive plan still
	// needs an approval where the apply policy asks for one.
	// +kubebuilder:default=false
	AllowDestructive bool `json:"allowDestructive,omitempty"`

	// DriftSeverity decides which differences count as drift worth applying:
	// every difference, or only the destructive ones.
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
	// Exclude is the managed scope the apply was computed under.
	Exclude []string `json:"exclude,omitempty"`

	// LockTimeout is how long an operation waits for the database's own lock
	// before giving up, so a busy database delays a run rather than stalling it
	// for the Job's whole deadline.
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('10m')",message="lockTimeout must be between 1s and 10m"
	LockTimeout metav1.Duration `json:"lockTimeout,omitempty"`

	// TransactionMode is how the statements are wrapped: all in one
	// transaction, one per file, or none at all. An engine that refuses a mode
	// decides over this rather than around it.
	// +kubebuilder:validation:Enum=all;file;none
	// +kubebuilder:default=file
	TransactionMode string `json:"transactionMode,omitempty"`

	// ProtectedTables fences declared row sets off from the declarative path.
	// A plan that would change a listed table is refused rather than rated, and
	// there is no override: an approval, allowDestructive and a permissive
	// severity are all answers to "how risky is this", and a fence is the
	// statement that no such answer exists for these rows. Where the change is
	// wanted, the entry goes, or the rows are written as a migration.
	//
	// An entry names a table, or a schema and a table, the way the declaration
	// does. Matching is Ptah's, which is case-insensitive, and an entry on a
	// table the artifact already agrees with refuses nothing -- which is what
	// lets a fence sit in a policy permanently.
	//
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`
	ProtectedTables []string `json:"protectedTables,omitempty"`
}

// ExecutionSpec exposes bounded scheduling and resource controls while
// withholding arbitrary Pod/container command customization.
type ExecutionSpec struct {
	// ActiveDeadlineSeconds is how long one operation Job may run before
	// Kubernetes ends it. An apply that hits this leaves an uncertain outcome,
	// which returns to observation rather than to a replay.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=900
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`

	// FailureRetryInterval is how long the controller waits after a failed
	// operation before trying the same one again.
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5s') && duration(self) <= duration('1h')",message="failureRetryInterval must be between 5s and 1h"
	FailureRetryInterval metav1.Duration `json:"failureRetryInterval,omitempty"`

	// ConnectTimeout bounds opening the database connection, so an unreachable
	// database fails in seconds rather than holding the Job to its deadline.
	// +kubebuilder:default="10s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('10m')",message="connectTimeout must be between 1s and 10m"
	ConnectTimeout metav1.Duration `json:"connectTimeout,omitempty"`

	// Resources are the requests and limits of the container that runs SQL.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// ServiceAccountName is the identity operation Pods run as. It is theirs
	// rather than the manager's, and it needs no Kubernetes permission at all.
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// ImagePullSecrets are the pull Secrets those Pods use.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	// NodeSelector restricts where operation Pods may be scheduled.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations are the taints those Pods tolerate.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity is scheduling affinity for those Pods.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// RuntimeClassName selects the container runtime they use. The admission
	// snapshot records what the cluster resolved, so a class that changed under
	// a claim is refused rather than run.
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
	// PriorityClassName is the scheduling priority they run at.
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// PodMetadata is what an operation Pod carries for the cluster around it:
	// a service mesh's opt-out annotation, the label a policy engine
	// requires, a team's own bookkeeping. Every operation Pod this resource
	// dispatches carries exactly these labels and annotations beside the
	// operator's own, and the admission snapshot binds them: a Pod that
	// arrives with more, fewer or different metadata is refused, so nothing
	// unplanned runs beside the database credential. Keys under ptah.run,
	// kubernetes.io and k8s.io are refused, so the operator's own labels, the
	// Job controller's and the built-in admission plugins' cannot be
	// redeclared, and nothing here can select an operation Pod into an
	// object the operator owns. Unset, nothing changes.
	// +optional
	PodMetadata *PodMetadataSpec `json:"podMetadata,omitempty"`
}

const (
	// MaxPodMetadataEntries bounds each of the two maps in PodMetadataSpec.
	MaxPodMetadataEntries = 16
	// MaxPodMetadataKeyLength is a qualified key at its longest: a 253-byte
	// prefix, the slash, and a 63-byte name.
	MaxPodMetadataKeyLength = 317
	// MaxPodLabelValueLength is what Kubernetes accepts for a label value.
	MaxPodLabelValueLength = 63
	// MaxPodAnnotationValueLength bounds one declared annotation value. With
	// MaxPodMetadataEntries and MaxPodMetadataKeyLength it bounds the whole
	// declaration to under 22 KiB of annotations and 6 KiB of labels.
	MaxPodAnnotationValueLength = 1024
)

// PodMetadataSpec is the bounded metadata operation Pods carry for a service
// mesh or a policy engine. Each map takes at most 16 entries. A key is a
// Kubernetes qualified name; a label value is what Kubernetes accepts for
// one, and an annotation value is at most 1024 bytes. Keys whose prefix is
// ptah.run, kubernetes.io or k8s.io, or a subdomain of one, are refused, and
// so are the bare controller-uid and job-name keys the Job controller sets.
type PodMetadataSpec struct {
	// Labels are added to every operation Pod beside the operator's own.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule=`self.all(key, key.size() <= 317 && (key.contains('/') ? (key.indexOf('/') <= 253 && key.size() - key.indexOf('/') <= 64) : key.size() <= 63) && key.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'))`,message="podMetadata.labels keys must be valid Kubernetes label keys"
	// +kubebuilder:validation:XValidation:rule=`self.all(key, !(key.startsWith('ptah.run/') || key.contains('.ptah.run/') || key.startsWith('kubernetes.io/') || key.contains('.kubernetes.io/') || key.startsWith('k8s.io/') || key.contains('.k8s.io/') || key == 'controller-uid' || key == 'job-name'))`,message="podMetadata.labels keys under ptah.run, kubernetes.io and k8s.io are reserved, and so are controller-uid and job-name"
	Labels map[string]PodLabelValue `json:"labels,omitempty"`
	// Annotations are added to every operation Pod beside the operator's
	// own.
	// +optional
	// +kubebuilder:validation:MaxProperties=16
	// +kubebuilder:validation:XValidation:rule=`self.all(key, key.size() <= 317 && (key.contains('/') ? (key.indexOf('/') <= 253 && key.size() - key.indexOf('/') <= 64) : key.size() <= 63) && key.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$'))`,message="podMetadata.annotations keys must be valid Kubernetes annotation keys"
	// +kubebuilder:validation:XValidation:rule=`self.all(key, !(key.startsWith('ptah.run/') || key.contains('.ptah.run/') || key.startsWith('kubernetes.io/') || key.contains('.kubernetes.io/') || key.startsWith('k8s.io/') || key.contains('.k8s.io/') || key == 'controller-uid' || key == 'job-name'))`,message="podMetadata.annotations keys under ptah.run, kubernetes.io and k8s.io are reserved, and so are controller-uid and job-name"
	Annotations map[string]PodAnnotationValue `json:"annotations,omitempty"`
}

// PodLabelValue is one declared label value: what Kubernetes accepts for a
// label value, bounded in the schema so a bad value is refused on the
// resource rather than on the Job the controller would have built from it.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type PodLabelValue string

// PodAnnotationValue is one declared annotation value, bounded in length and
// otherwise free, as Kubernetes leaves it.
// +kubebuilder:validation:MaxLength=1024
type PodAnnotationValue string

// PtahSchemaStatus records only credential-free reconciliation evidence.
type PtahSchemaStatus struct {
	// ObservedGeneration is the spec generation this status describes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is where the resource stands, as one word for a reader. The
	// conditions below are what a decision reads: a resource legitimately
	// passes through several phases while one refusal stays true.
	Phase ReconciliationPhase `json:"phase,omitempty"`

	// ExecutionBinding is the durable identity of the execution epoch
	// authorized to produce new reconciliation evidence. Retained evidence stays
	// historical until refreshed. Epoch changes whenever a bound component
	// changes, including a rollback to identical values; a manager upgrade
	// that changes no bound component keeps it.
	ExecutionBinding *ExecutionBindingStatus `json:"executionBinding,omitempty"`

	// Source is what the desired artifact resolved and verified to.
	Source SchemaSourceStatus `json:"source,omitempty"`
	// Target is what the last observation found in the database.
	Target TargetStatus `json:"target,omitempty"`
	// Plan is the published plan waiting to run, where there is one.
	Plan *CurrentPlanStatus `json:"plan,omitempty"`
	// Applied is the last apply that was independently observed to have
	// converged, which is a different claim from a Job that exited zero.
	Applied *AppliedStatus `json:"applied,omitempty"`

	// PendingObservation is durable proof work created after an Apply Job may
	// have mutated the database. It is independent of Phase so retries and
	// suspension cannot accidentally permit another mutation first.
	PendingObservation *PendingObservationStatus `json:"pendingObservation,omitempty"`
	// PendingLockRelease keeps the exact Lease owner and epoch durable until an
	// idempotent release succeeds. It closes the manager-crash window between a
	// terminal status transition and clearing the owner-neutral Lease.
	PendingLockRelease *TargetLockReleaseStatus `json:"pendingLockRelease,omitempty"`
	// PendingBindingRetirement is the cleanup an execution-binding rotation
	// still owes the epoch it replaced. The status write that installs the new
	// epoch writes it, and it is removed once nothing in it is left. Another
	// rotation waits until it is gone.
	PendingBindingRetirement *BindingRetirementStatus `json:"pendingBindingRetirement,omitempty"`

	// ActiveOperation is the claim for the operation in flight. It is written
	// before the Job exists, which is what lets the controller tell a Job it
	// created from one it has not created yet.
	ActiveOperation *ActiveOperationStatus `json:"activeOperation,omitempty"`

	// LastAttemptTime is when the controller last tried to do something.
	LastAttemptTime *metav1.Time `json:"lastAttemptTime,omitempty"`
	// LastSuccessfulReconciliation is when it last completed a cycle with
	// nothing left to do.
	LastSuccessfulReconciliation *metav1.Time `json:"lastSuccessfulReconciliation,omitempty"`
	// NextReconciliationTime is the durable earliest time for the next
	// scheduled read-only reconciliation. Event-driven safety work may run
	// sooner.
	NextReconciliationTime *metav1.Time `json:"nextReconciliationTime,omitempty"`

	// Conditions are the readable verdicts: whether the engine is supported,
	// the artifact resolved and verified, the database was reachable, drift was
	// found, a plan is ready, an approval is required, the schema is in sync,
	// and whether the last reconciliation failed.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ExecutionBindingStatus identifies one uninterrupted execution epoch: the
// components that decide what a plan means when it runs. The explicit
// component fields are audit evidence; Epoch prevents approvals from becoming
// current again after a later rollback.
//
// The manager's own image and revision, and the runner image built beside
// it, are deliberately absent. They are recorded on every Job and plan, and a
// release that changes only them -- a patch or security fix -- keeps the
// epoch, so a pending approval stays valid and a plan stays applicable. What
// the runner enforces is bound through RunnerProtocolVersion instead, which
// changes whenever that enforcement does.
type ExecutionBindingStatus struct {
	// Epoch is this binding's identity. It changes whenever a component below
	// changes, a rollback to identical versions included, so evidence from
	// before such a rollout is historical rather than current.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	Epoch string `json:"epoch"`

	// ControllerStateVersion versions manager-side reconciliation semantics
	// independently of the data-plane runner protocol.
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32 `json:"controllerStateVersion"`

	// PtahVersion is the Ptah build this epoch executes with.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PtahVersion string `json:"ptahVersion"`
	// ExecutorImage is the digest-pinned image carrying that build.
	// +kubebuilder:validation:MinLength=1
	ExecutorImage string `json:"executorImage"`
	// RunnerProtocolVersion is the runner protocol this epoch binds: what the
	// runner enforces inside the Pod and the result frame it returns.
	// +kubebuilder:validation:Minimum=1
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`
}

// ManagerRecord names one release of the manager: its image, its revision and
// the runner image built from the same source. It is audit evidence and binds
// nothing -- a release that changes only these values keeps every plan and
// approval -- so it is written where a reader needs to know which build did
// something after the Job that ran it has been collected.
type ManagerRecord struct {
	// ControllerImage is the digest-pinned manager image.
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=512
	ControllerImage string `json:"controllerImage"`
	// ControllerRevision is the source revision the manager was built from.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`
	ControllerRevision string `json:"controllerRevision"`
	// RunnerImage is the digest-pinned runner image the manager installed in
	// the task Pod.
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=512
	RunnerImage string `json:"runnerImage"`
}

// TargetLockReleaseStatus is the complete credential-free request required to
// retry one exact database-realm Lease release after a manager restart.
type TargetLockReleaseStatus struct {
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// CoordinationDigest is the realm whose lock is still to be released.
	CoordinationDigest string `json:"coordinationDigest"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// OperationID is the operation that took it.
	OperationID string `json:"operationID"`
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=86460
	// LeaseDurationSeconds is how long it was taken for.
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds"`
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	// LeaseEpoch identifies that acquisition, so a release cannot free a lock
	// somebody else acquired in the meantime.
	LeaseEpoch string `json:"leaseEpoch"`
}

// BindingRetirementStatus is what retiring one execution-binding epoch still
// owes. Each optional field is one obligation and is removed when it is met,
// so what is present is the state of the cleanup: a plan whose approvals are
// still to be marked stale, a Job whose UID is still to be adopted, a Job
// whose cleanup TTL is still to be set.
//
// Nothing here authorizes anything. The write that created the record also
// moved the epoch every plan, approval and claim is checked against, so the
// record only says which leftovers of the old epoch are this resource's to
// tidy.
type BindingRetirementStatus struct {
	// RetiredEpoch is the epoch the rotation replaced. The plan and the Job
	// this record names belong to it, and the controller touches a Job only
	// when the Job carries this epoch.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	RetiredEpoch string `json:"retiredEpoch"`

	// Plan is the retired epoch's plan, while approvals that name it are
	// still to be marked stale.
	// +optional
	Plan *RetiredPlanReference `json:"plan,omitempty"`

	// Job is the Job the retired epoch's claim dispatched, while its cleanup
	// is still to be scheduled.
	// +optional
	Job *RetiredJobStatus `json:"job,omitempty"`
}

// RetiredPlanReference names a retired PtahSchemaPlan the way an approval
// names it: by name, UID and fingerprint.
type RetiredPlanReference struct {
	// Name of the PtahSchemaPlan.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// UID it had.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
	// Fingerprint is its approval identity.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Fingerprint string `json:"fingerprint"`
}

// RetiredJobStatus is the Job a retired epoch's claim dispatched. The claim
// that holds the rest of its identity stays where it was: an Apply in
// status.pendingObservation, a read-only operation in status.activeOperation
// until its Job is cleaned up.
type RetiredJobStatus struct {
	// Operation is the claim that dispatched the Job.
	// +kubebuilder:validation:Enum=Resolve;Verify;Observe;Plan;Apply
	Operation OperationType `json:"operation"`
	// Name is the Job's deterministic name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// UID is the Job's UID once it is known. It is absent while a create the
	// retired claim started may still commit; the controller adopts it from a
	// Job under Name that carries the claim's exact envelope.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid,omitempty"`
}

// AdmissionObjectBinding identifies one API object whose credential-free
// contents contributed to the resolved Pod admission envelope.
type AdmissionObjectBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// Name of the cluster object this snapshot was read from.
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// UID it had, so a recreated object is a different one.
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
	// Object is the LimitRange this was read from.
	Object AdmissionObjectBinding `json:"object"`
	// +kubebuilder:validation:MaxProperties=64
	// DefaultRequests are the requests it would add to a container that asks
	// for none.
	DefaultRequests map[corev1.ResourceName]resource.Quantity `json:"defaultRequests,omitempty"`
	// +kubebuilder:validation:MaxProperties=64
	// DefaultLimits are the limits it would add.
	DefaultLimits map[corev1.ResourceName]resource.Quantity `json:"defaultLimits,omitempty"`
}

// RuntimeClassAdmissionSnapshot records the exact scheduling and overhead
// mutation selected before dispatch. Handler is deliberately retained as
// credential-free audit evidence even though it is not copied into PodSpec.
type RuntimeClassAdmissionSnapshot struct {
	// Object is the RuntimeClass this was read from, by name and UID.
	Object AdmissionObjectBinding `json:"object"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// Handler is the runtime handler it names.
	Handler string `json:"handler"`
	// OverheadDefined distinguishes an absent RuntimeClass overhead stanza from
	// a present but empty one; Kubernetes admission preserves that distinction.
	// OverheadDefined separates a class with no overhead from one whose
	// overhead is zero.
	OverheadDefined bool `json:"overheadDefined,omitempty"`
	// +kubebuilder:validation:MaxProperties=64
	// Overhead is the per-Pod resource overhead the class adds.
	Overhead map[corev1.ResourceName]resource.Quantity `json:"overhead,omitempty"`
	// +kubebuilder:validation:MaxProperties=64
	// NodeSelector is the scheduling the class forces.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// Tolerations are the tolerations it adds.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// PriorityClassAdmissionSnapshot records the exact values injected by the
// Priority admission plugin. Object is absent only when the cluster has no
// global default and the Job does not request a named PriorityClass.
type PriorityClassAdmissionSnapshot struct {
	// Object is the PriorityClass this was read from, where a class was named.
	Object *AdmissionObjectBinding `json:"object,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	// Name is that class, as the Pod requests it.
	Name string `json:"name,omitempty"`
	// Value is the priority it resolved to.
	Value int32 `json:"value"`
	// +kubebuilder:validation:Enum=Never;PreemptLowerPriority
	// PreemptionPolicy is what that class says about preempting others.
	PreemptionPolicy *corev1.PreemptionPolicy `json:"preemptionPolicy,omitempty"`
}

// ServiceAccountAdmissionSnapshot binds the non-secret ServiceAccount fields
// that built-in admission may copy into a Pod.
type ServiceAccountAdmissionSnapshot struct {
	// Object is the ServiceAccount the Pod runs as.
	Object AdmissionObjectBinding `json:"object"`
	// +kubebuilder:validation:MaxItems=64
	// ImagePullSecrets are the pull Secrets it contributes to the Pod.
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// PodAdmissionSnapshot is the bounded, credential-free mutation envelope for
// one active operation. Digest covers every other field using canonical JSON.
// A controller restart validates this persisted snapshot instead of rereading
// resources whose later mutations must not reinterpret the operation.
type PodAdmissionSnapshot struct {
	// +kubebuilder:validation:Enum=v1
	// Version is the snapshot format this record was written in.
	Version string `json:"version"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// Digest covers this whole snapshot, so a Pod can be checked against it
	// without re-reading the cluster objects it describes.
	Digest string `json:"digest"`
	// TemplateDigest binds the canonical, API-defaulted pre-admission Job Pod
	// template. The self-referential snapshot annotation and four exact
	// API-server-generated Job identity labels are omitted and validated
	// separately against the current Job name and UID.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// TemplateDigest covers the Pod template the operator asked for, before the
	// cluster's own admission had a chance to change it.
	TemplateDigest string `json:"templateDigest"`

	// ServiceAccount is the identity the Pod runs as, as it resolved.
	ServiceAccount ServiceAccountAdmissionSnapshot `json:"serviceAccount"`
	// +kubebuilder:validation:MaxItems=32
	// LimitRanges are the namespace defaults that would be applied to the Pod.
	LimitRanges []LimitRangeAdmissionSnapshot `json:"limitRanges,omitempty"`
	// RuntimeClass is the container runtime it resolved to, where one is named.
	RuntimeClass *RuntimeClassAdmissionSnapshot `json:"runtimeClass,omitempty"`
	// PriorityClass is the scheduling priority it resolved to.
	PriorityClass PriorityClassAdmissionSnapshot `json:"priorityClass"`
	// DefaultTolerationsEnabled records whether kube-apiserver runs the
	// DefaultTolerationSeconds admission plugin. It and the two values below
	// say what that plugin does, so a toleration the Pod did not ask for is
	// recognized rather than refused.
	DefaultTolerationsEnabled bool `json:"defaultTolerationsEnabled"`
	// +kubebuilder:validation:Minimum=0
	// DefaultNotReadyTolerationSeconds is that plugin's not-ready value.
	DefaultNotReadyTolerationSeconds int64 `json:"defaultNotReadyTolerationSeconds"`
	// +kubebuilder:validation:Minimum=0
	// DefaultUnreachableTolerationSeconds is that plugin's unreachable value.
	DefaultUnreachableTolerationSeconds int64 `json:"defaultUnreachableTolerationSeconds"`
	// ExtendedResourceTolerationEnabled records whether kube-apiserver runs the
	// ExtendedResourceToleration admission plugin, which adds a toleration per
	// extended resource a Pod requests.
	ExtendedResourceTolerationEnabled bool `json:"extendedResourceTolerationEnabled"`
	// AlwaysPullImagesEnabled records whether kube-apiserver runs the
	// AlwaysPullImages admission plugin, which rewrites every imagePullPolicy
	// to Always.
	AlwaysPullImagesEnabled bool `json:"alwaysPullImagesEnabled"`
}

// SchemaSourceStatus binds the requested reference to verified immutable data.
type SchemaSourceStatus struct {
	// RequestedReference is what spec.desired asked for, tag and all.
	RequestedReference string `json:"requestedReference,omitempty"`
	// ResolvedReference is the same artifact with the tag replaced by the
	// digest it resolved to. Every later read uses this.
	ResolvedReference string `json:"resolvedReference,omitempty"`
	// Digest is that digest on its own.
	Digest string `json:"digest,omitempty"`
	// MediaType is the manifest's media type.
	MediaType string `json:"mediaType,omitempty"`
	// ArtifactType says which format this is: a declared schema or a versioned
	// migration directory.
	ArtifactType string `json:"artifactType,omitempty"`
	// Size is the manifest's size in bytes.
	Size int64 `json:"size,omitempty"`
	// Verified says the verification policy accepted it. It goes false again
	// when the policy changes, because the old answer was about the old rules.
	Verified bool `json:"verified,omitempty"`

	// VerificationPolicyUID is the policy object that accepted it.
	VerificationPolicyUID types.UID `json:"verificationPolicyUID,omitempty"`
	// VerificationPolicyDigest is that policy's content at the time.
	VerificationPolicyDigest string `json:"verificationPolicyDigest,omitempty"`
	// ResolvedAt is when the tag was last resolved.
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
	// VerifiedAt is when the policy last accepted it.
	VerifiedAt *metav1.Time `json:"verifiedAt,omitempty"`

	// ConfigMap is the source ConfigMap the files were last read from, for a
	// ConfigMap source. A ConfigMap source has no verification policy, so
	// verified stays false; the snapshot below is what later operations read.
	// +optional
	ConfigMap *ConfigMapSourceRevision `json:"configMap,omitempty"`
	// Snapshot is the immutable copy of the selected files, and their content
	// digest, for a ConfigMap source.
	// +optional
	Snapshot *SourceSnapshotBinding `json:"snapshot,omitempty"`
	// Git is the commit a Git source last resolved to, and the content
	// digest of the selected files there. A Git source has no verification
	// policy, so verified stays false.
	// +optional
	Git *GitSourceRevision `json:"git,omitempty"`
	// SourcePolicy is the source policy the last resolution was checked
	// against, when the spec names one.
	// +optional
	SourcePolicy *SourcePolicyBinding `json:"sourcePolicy,omitempty"`
}

// DriftFindingStatus is a bounded aggregate from the native drift report. It
// intentionally excludes object names, SQL, schema literals, and raw diffs.
//
// The data_rows_ categories say that declared reference rows differ and by how
// many, and that is all a row contributes to this object: no key, no column
// name and no value. A reader who needs to know which rows reads the plan,
// which is data access and documented as such.
type DriftFindingStatus struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=153
	// +kubebuilder:validation:Pattern=`^(async_replications_added|async_replications_modified|async_replications_removed|columns_added|columns_modified|columns_removed|constraints_added|constraints_removed|coordination_nodes_added|coordination_nodes_modified|coordination_nodes_removed|data_rows_deleted|data_rows_inserted|data_rows_updated|enum_values_added|enum_values_removed|enums_added|enums_removed|extensions_added|extensions_modified|extensions_removed|external_data_sources_added|external_data_sources_changed|external_data_sources_removed|external_tables_added|external_tables_changed|external_tables_removed|functions_added|functions_modified|functions_removed|indexes_added|indexes_removed|resource_pool_classifiers_added|resource_pool_classifiers_modified|resource_pool_classifiers_removed|resource_pools_added|resource_pools_modified|resource_pools_removed|rls_enabled_tables_added|rls_enabled_tables_removed|rls_force_added|rls_force_removed|rls_policies_added|rls_policies_modified|rls_policies_removed|roles_added|roles_modified|roles_removed|schemas_added|schemas_modified|schemas_removed|secrets_added|secrets_removed|secrets_rotated|table_constraints_added|table_constraints_removed|tables_added|tables_removed|topic_consumers_removed|topics_added|topics_modified|topics_removed|transfers_added|transfers_modified|transfers_removed|undecided|unique_protections_removed|vector_dimension_changed|feature_(changes|access_(widened|narrowed|unchanged|unknown))(:[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(/[A-Za-z0-9_-][A-Za-z0-9._-]*)+)?)$`
	// Category is the kind of difference, never the object it was found in:
	// a table name is part of the schema, and the status does not carry it.
	// A feature category may name the type of object after a colon, as in
	// feature_changes:ptah.run/pgextension/extension-change; a type is not an
	// object either. The pattern admits every category an earlier release
	// stored, so a summary written before an upgrade still validates.
	Category string `json:"category"`
	// +kubebuilder:validation:Minimum=1
	// Count is how many differences of that kind the report held.
	Count int32 `json:"count"`
	// +kubebuilder:validation:Enum=safe;info;warning;error;destructive
	// Severity is how the category rates.
	Severity string `json:"severity"`
}

// TargetStatus identifies the Secret value and observed schema without
// disclosing either the connection string or its credentials.
type TargetStatus struct {
	// Engine is the database engine this target speaks.
	Engine DatabaseEngine `json:"engine,omitempty"`
	// CoordinationDigest is the realm this target serializes against, so two
	// resources pointed at one database take turns.
	CoordinationDigest string `json:"coordinationDigest,omitempty"`
	// IdentityDigest identifies the database without carrying anything that
	// could reach it.
	IdentityDigest string `json:"identityDigest,omitempty"`
	// DriftReportDigest is the observed state the last plan was computed from.
	DriftReportDigest string `json:"driftReportDigest,omitempty"`
	// LastObservedAt is when that observation ran.
	LastObservedAt *metav1.Time `json:"lastObservedAt,omitempty"`
	// HighestDriftSeverity is the worst category the report found. It is set
	// whenever the report found drift and empty when it found none, so it also
	// says which. A difference the report has no category for -- a grant, a
	// default privilege, a view or a trigger -- is drift that reads safe here,
	// with no findings and a zero count; the plan says what it changes.
	HighestDriftSeverity string `json:"highestDriftSeverity,omitempty"`
	// DriftFindingCount is how many findings the report held: the sum of the
	// counts below. Drift in no category counts none.
	DriftFindingCount int32 `json:"driftFindingCount,omitempty"`
	// DriftFindings contains only category-level aggregates, one per category
	// the report found. The list is never truncated: it is keyed by category,
	// and the bound holds every category in the vocabulary.
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=category
	DriftFindings []DriftFindingStatus `json:"driftFindings,omitempty"`
}

// CurrentPlanStatus is a compact reference to an immutable PtahSchemaPlan.
type CurrentPlanStatus struct {
	// Name of the PtahSchemaPlan this record is about.
	Name string `json:"name"`
	// UID it had when this record was written.
	UID types.UID `json:"uid"`

	// Fingerprint is the plan's complete approval identity, and the fields
	// below are that identity spelled out. They are copied here so a reader --
	// and an audit -- can see what is waiting without fetching the plan.
	Fingerprint string `json:"fingerprint"`
	// ContentDigest is the digest of the plan bytes.
	ContentDigest string `json:"contentDigest"`
	// ArtifactDigest is the OCI artifact the plan was computed from. A plan
	// computed from another source binds source instead.
	// +optional
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	// Source is the source a plan computed from a ConfigMap or a Git commit
	// binds in place of artifactDigest.
	// +optional
	Source *PlanSourceBinding `json:"source,omitempty"`
	// CoordinationDigest is the database realm it takes its turn in.
	CoordinationDigest string `json:"coordinationDigest"`
	// TargetIdentityDigest is the database it was computed against.
	TargetIdentityDigest string `json:"targetIdentityDigest"`
	// ActualStateFingerprint is the observed state it was planned from.
	ActualStateFingerprint string `json:"actualStateFingerprint"`
	// DesiredStateFingerprint is the state the artifact declared.
	DesiredStateFingerprint string `json:"desiredStateFingerprint"`
	// PolicyFingerprint is the spec.policy it was computed under.
	PolicyFingerprint string `json:"policyFingerprint"`
	// VerificationPolicyUID is the policy object that accepted the artifact,
	// for an OCI plan.
	// +optional
	VerificationPolicyUID types.UID `json:"verificationPolicyUID,omitempty"`
	// VerificationPolicyDigest is that policy's content at the time.
	// +optional
	VerificationPolicyDigest string `json:"verificationPolicyDigest,omitempty"`
	// ExecutionBindingID is the execution epoch it belongs to.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID"`
	// ControllerImage is the digest-pinned manager that published it,
	// recorded and not bound.
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage"`
	// ControllerRevision is that manager's revision.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`
	ControllerRevision string `json:"controllerRevision"`
	// ControllerStateVersion is the state semantics it writes.
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32 `json:"controllerStateVersion"`
	// PtahVersion is the Ptah build that computed the plan.
	PtahVersion string `json:"ptahVersion"`
	// ExecutorImage is the digest-pinned image that ran it.
	ExecutorImage string `json:"executorImage"`
	// RunnerImage is the runner image of the manager that published it,
	// recorded and not bound.
	RunnerImage string `json:"runnerImage"`
	// RunnerProtocolVersion is the runner protocol the plan binds.
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`
	// Destructive says the plan drops or rewrites something.
	Destructive bool `json:"destructive"`
	// PrivilegeChanges names the kinds of authority the plan changes, as the
	// plan records them. Any entry means an approval is required even when
	// spec.policy.apply is Always.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	PrivilegeChanges []PrivilegeChange `json:"privilegeChanges,omitempty"`
	// StatementCount is how many statements it holds. The statements
	// themselves are not here: read them with kubectl ptah plan.
	StatementCount int32 `json:"statementCount"`
	// CreatedAt is the plan object's own creation time, copied like every
	// other field here, so an audit of this record and of the plan it names
	// cannot disagree about when the plan came into being.
	CreatedAt metav1.Time `json:"createdAt"`

	// Approval is the decision that authorized this plan, where one was made.
	Approval *ConsumedApprovalStatus `json:"approval,omitempty"`
}

// ConsumedApprovalStatus records the immutable approval object and identity.
type ConsumedApprovalStatus struct {
	// Name of the approval that authorized the apply.
	Name string `json:"name"`
	// UID it had, so a recreated approval is not read as the same decision.
	UID types.UID `json:"uid"`
	// Approver is who the API server authenticated.
	Approver ApprovalIdentity `json:"approver"`
	// ApprovedAt is when the decision was stamped.
	ApprovedAt metav1.Time `json:"approvedAt"`
}

// AppliedStatus is written only after post-apply observation proves convergence.
type AppliedStatus struct {
	// ArtifactDigest is the OCI artifact that was applied.
	// +optional
	ArtifactDigest string `json:"artifactDigest,omitempty"`
	// Source is the source that was applied, for a plan computed from a
	// ConfigMap or a Git commit.
	// +optional
	Source *PlanSourceBinding `json:"source,omitempty"`
	// PlanRef names the stored plan this apply ran. It is what a reader
	// addresses to see the SQL that was applied. It does not replace
	// PlanFingerprint: the reference says which object to read and the
	// fingerprint says whether the object read is the one this record was
	// written for.
	PlanRef ImmutableObjectReference `json:"planRef"`
	// PlanFingerprint says whether the plan object read today is the one this
	// record was written for.
	PlanFingerprint string `json:"planFingerprint"`
	// CoordinationDigest is the realm the apply held while it ran.
	CoordinationDigest string `json:"coordinationDigest"`
	// TargetIdentityDigest is the database it converged.
	TargetIdentityDigest string `json:"targetIdentityDigest"`
	// ExecutionBindingID is the epoch the apply ran under.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID"`
	// ControllerImage is the digest-pinned manager that published the plan
	// this apply ran. The manager that dispatched the Job is DispatchedBy.
	// +kubebuilder:validation:Pattern=`^[^[:space:]@]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage"`
	// ControllerRevision is that manager's revision.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[^[:space:][:cntrl:]]([^[:cntrl:]]*[^[:space:][:cntrl:]])?$`
	ControllerRevision string `json:"controllerRevision"`
	// ControllerStateVersion is the state semantics it wrote.
	// +kubebuilder:validation:Minimum=1
	ControllerStateVersion int32 `json:"controllerStateVersion"`
	// PtahVersion is the Ptah build that executed the statements.
	PtahVersion string `json:"ptahVersion"`
	// ExecutorImage is the digest-pinned image it ran in.
	ExecutorImage string `json:"executorImage"`
	// RunnerImage is the runner image of the manager that published the plan.
	RunnerImage string `json:"runnerImage"`
	// RunnerProtocolVersion is the runner protocol the apply ran under.
	RunnerProtocolVersion int32 `json:"runnerProtocolVersion"`
	// DispatchedBy is the manager that built and dispatched the Apply Job,
	// read from the Job's Pod template when the Apply was harvested. It can
	// differ from the publisher above: a later release of the manager that
	// shares the execution binding applies the plans an earlier one
	// published. It is absent when the Apply was settled without its Job.
	// +optional
	DispatchedBy *ManagerRecord `json:"dispatchedBy,omitempty"`
	// CompletedAt is when convergence was independently observed, not when the
	// Job exited.
	CompletedAt metav1.Time `json:"completedAt"`
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
	// Outcome is what is known about the apply this proof is for: that it
	// succeeded, or that nobody can say.
	Outcome PendingObservationOutcome `json:"outcome"`
	// ApplyOperationID is the apply attempt this proof belongs to.
	ApplyOperationID string `json:"applyOperationID"`
	// ApplyJobName and ApplyJobUID identify the Kubernetes Job independently
	// of mutable labels so every exact-owner Pod can be tracked until the
	// immutable execution horizon has elapsed.
	// +kubebuilder:validation:MaxLength=253
	ApplyJobName string `json:"applyJobName,omitempty"`
	// ApplyJobUID is that Job's UID.
	ApplyJobUID types.UID `json:"applyJobUID,omitempty"`
	// AdmissionSnapshot retains the exact pre-admission Pod template identity
	// after ActiveOperation is cleared. Apply Job cleanup after an
	// execution-binding change fails closed when this evidence is absent.
	AdmissionSnapshot *PodAdmissionSnapshot `json:"admissionSnapshot,omitempty"`
	// ApplyPodUIDs and ApplyPodCount preserve the terminal Pod evidence seen at
	// the mutation boundary. More than one Pod always forces outcome-unknown
	// proof even for a one-shot Job.
	// +kubebuilder:validation:MaxItems=8
	ApplyPodUIDs []types.UID `json:"applyPodUIDs,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// ApplyPodCount is how many of them there were.
	ApplyPodCount int32 `json:"applyPodCount,omitempty"`
	// ApplyGeneration is the spec generation the apply was dispatched for, so
	// a newer desired state does not inherit this proof.
	ApplyGeneration int64 `json:"applyGeneration"`
	// ObserveAfter delays proof when the Kubernetes Job identity or create
	// result is uncertain. Until this time, the original mutating Pod could
	// still be within its immutable active deadline.
	ObserveAfter *metav1.Time `json:"observeAfter,omitempty"`

	// Plan is the plan the apply carried out, kept here after the active
	// operation is cleared so the proof knows what it is proving. Its manager
	// fields name the manager that published it.
	Plan CurrentPlanStatus `json:"plan"`
	// DispatchedBy is the manager that built and dispatched the Apply Job,
	// read from the Job's Pod template when the Apply was harvested, and
	// carried into status.applied when the proof completes. It is absent when
	// the Apply was settled without its Job.
	// +optional
	DispatchedBy *ManagerRecord `json:"dispatchedBy,omitempty"`
	// Target is the key-free binding the proof reads the database through.
	Target DatabaseTargetBinding `json:"target"`
	// CoordinationDigest is the realm the apply held, kept so the proof runs
	// under the same lock rather than racing somebody else's turn.
	CoordinationDigest string `json:"coordinationDigest"`
	// Source is the artifact access the proof needs to re-read the desired
	// state. It holds Kubernetes selectors, never Secret contents.
	Source SourceAccessBinding `json:"source"`
	// Dev is the scratch database the proof may use.
	Dev *DatabaseTargetRef `json:"dev,omitempty"`
	// PlanRequired records that the raw drift read completed and the same
	// immutable proof inputs now require authoritative managed-scope planning.
	PlanRequired bool `json:"planRequired,omitempty"`

	// Exclude is the managed scope the apply was computed under.
	Exclude []string `json:"exclude,omitempty"`
	// ProtectedTables is the fence the in-flight plan was computed under. A
	// policy edit while a plan is pending must not let it execute against a
	// fence it never saw.
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	ProtectedTables []string `json:"protectedTables,omitempty"`
	// DriftSeverity is the severity the proof reads at.
	DriftSeverity string `json:"driftSeverity,omitempty"`
	// ConnectTimeout is the connect timeout the proof is dispatched with.
	ConnectTimeout metav1.Duration `json:"connectTimeout,omitempty"`
	// LockTimeout is the database lock timeout it is dispatched with.
	LockTimeout metav1.Duration `json:"lockTimeout,omitempty"`

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
	// Type is the operation this claim authorizes: resolving the artifact,
	// verifying it, observing the database, planning, or applying.
	Type OperationType `json:"type"`
	// ID is this attempt's identity, distinct from every other attempt.
	ID string `json:"id"`
	// InputFingerprint is everything the claim was computed from. A changed
	// input produces a new claim rather than reusing this one.
	InputFingerprint string `json:"inputFingerprint"`
	// JobName is the Job this claim authorizes, named before it is created.
	JobName string `json:"jobName"`
	// JobUID is that Job's UID once it exists. A Job with the right name and
	// another UID is somebody else's.
	JobUID types.UID `json:"jobUID,omitempty"`
	// StartedAt is when the claim was written.
	StartedAt metav1.Time `json:"startedAt"`
	// Attempt counts this claim among the retries of the same operation.
	Attempt int32 `json:"attempt"`
	// ExecutionBindingID binds every Job and result to the durable evidence
	// epoch that authorized its claim.
	// +kubebuilder:validation:Pattern=`^v1-[0-9a-f]{32}$`
	ExecutionBindingID string `json:"executionBindingID"`

	// AdmissionSnapshot is persisted before dispatch and is bound into the Job
	// and Pod template annotations. It permits only modeled, safe built-in
	// admission mutations while retaining exact validation for executable and
	// security-sensitive Pod fields.
	AdmissionSnapshot *PodAdmissionSnapshot `json:"admissionSnapshot,omitempty"`
	// AdmissionSnapshotRefreshed records that this claim's admission snapshot
	// was resolved a second time, because the Job template this manager
	// builds differed from the one the snapshot recorded before anything was
	// dispatched. That happens once, when a manager release that shares the
	// execution binding takes over an undispatched claim. It happens at most
	// once per claim: a template that differs again comes from a builder that
	// does not build the same Job twice, and the claim is retired instead.
	AdmissionSnapshotRefreshed bool `json:"admissionSnapshotRefreshed,omitempty"`

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
	// TerminationGracePeriodSeconds is how long the operation Pod is given to
	// stop on its own before it is killed.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// Plan and Apply operations persist their credential-free lock binding so
	// later spec changes cannot redirect or shorten protection for a running Job.
	CoordinationDigest string `json:"coordinationDigest,omitempty"`
	// TargetIdentityDigest is the database that claim is for.
	TargetIdentityDigest string `json:"targetIdentityDigest,omitempty"`
	// Target is the key-free binding the Job resolves its credential through.
	Target *DatabaseTargetBinding `json:"target,omitempty"`
	// ObservationExclude is the managed scope the operation was given, copied
	// so a later spec edit cannot change what a running Job was asked to read.
	ObservationExclude []string `json:"observationExclude,omitempty"`
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	// ObservationProtectedTables is the fence the operation was dispatched
	// under, so an edit to the policy cannot reach a Job already running.
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	ObservationProtectedTables []string `json:"observationProtectedTables,omitempty"`
	// ObservationSeverity is the drift severity it was asked to report.
	ObservationSeverity string `json:"observationSeverity,omitempty"`
	// ObservationDev is the scratch database it may use, where one is
	// configured.
	ObservationDev *DatabaseTargetRef `json:"observationDev,omitempty"`
	// ObservationConnectTimeout is the connect timeout it was dispatched with.
	ObservationConnectTimeout metav1.Duration `json:"observationConnectTimeout,omitempty"`
	// ObservationLockTimeout is the database lock timeout it was dispatched
	// with.
	ObservationLockTimeout metav1.Duration `json:"observationLockTimeout,omitempty"`
	// VerificationPolicyUID and VerificationPolicyDigest bind a Verify Job to
	// the immutable ConfigMap version inspected before dispatch.
	VerificationPolicyUID types.UID `json:"verificationPolicyUID,omitempty"`
	// VerificationPolicyDigest is that ConfigMap's content at dispatch.
	VerificationPolicyDigest string `json:"verificationPolicyDigest,omitempty"`
	// Source snapshots artifact access for mandatory post-Apply observation.
	// It contains Kubernetes selectors only, never Secret contents.
	Source *SourceAccessBinding `json:"source,omitempty"`
	// LeaseDurationSeconds is how long the database lock was taken for.
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=86460
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds,omitempty"`
	// LeaseEpoch is persisted before dispatch. A different epoch invalidates
	// every result produced by this operation.
	LeaseEpoch string `json:"leaseEpoch,omitempty"`
	// LeaseContinuityLost is set before any result can be harvested when the
	// persisted epoch no longer owns an uninterrupted lock interval.
	LeaseContinuityLost bool `json:"leaseContinuityLost,omitempty"`

	// PlanSealPublicKeyDigest is the digest of the manager public key a Plan
	// Job's runner was given to seal its plan payload, persisted immediately
	// before the Job is created. A manager whose own current key no longer
	// matches this digest -- because it restarted and generated a new one --
	// cannot open that Job's result and retries instead of waiting: Plan is
	// read-only, so a fresh attempt sealed to the current key costs nothing
	// the first attempt did not already cost. Plan operations only.
	PlanSealPublicKeyDigest string `json:"planSealPublicKeyDigest,omitempty"`
}

const (
	ConditionArtifactResolved     = "ArtifactResolved"
	ConditionArtifactVerified     = "ArtifactVerified"
	ConditionEngineSupported      = "EngineSupported"
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

// ConditionReason is the stable, machine-readable reason vocabulary emitted
// by the schema controller. Messages may gain detail, but these values are API
// contracts and must not be repurposed.
type ConditionReason string

const (
	ReasonActive                       ConditionReason = "Active"
	ReasonApplyDisabled                ConditionReason = "ApplyDisabled"
	ReasonApplyOutcomeUnknown          ConditionReason = "ApplyOutcomeUnknown"
	ReasonApplyPending                 ConditionReason = "ApplyPending"
	ReasonApprovalRevoked              ConditionReason = "ApprovalRevoked"
	ReasonApprovedPlan                 ConditionReason = "ApprovedPlan"
	ReasonArtifactUnverified           ConditionReason = "ArtifactUnverified"
	ReasonAwaitingApproval             ConditionReason = "AwaitingApproval"
	ReasonCommitResolved               ConditionReason = "CommitResolved"
	ReasonConfigurationError           ConditionReason = "ConfigurationError"
	ReasonConvergedAfterUnknownOutcome ConditionReason = "ConvergedAfterUnknownOutcome"
	ReasonCurrentPlan                  ConditionReason = "CurrentPlan"
	ReasonDesiredStateChanged          ConditionReason = "DesiredStateChanged"
	ReasonDestructiveChangesDisabled   ConditionReason = "DestructiveChangesDisabled"
	ReasonDigestPinned                 ConditionReason = "DigestPinned"
	ReasonDispatchCommitted            ConditionReason = "DispatchCommitted"
	ReasonExecutionBindingChanged      ConditionReason = "ExecutionBindingChanged"
	ReasonInputsChanged                ConditionReason = "InputsChanged"
	ReasonInSync                       ConditionReason = "InSync"
	ReasonJobCompleted                 ConditionReason = "JobCompleted"
	ReasonLeaseContinuityLost          ConditionReason = "LeaseContinuityLost"
	ReasonNoChanges                    ConditionReason = "NoChanges"
	ReasonNotRequired                  ConditionReason = "NotRequired"
	ReasonObserved                     ConditionReason = "Observed"
	ReasonOperationFailed              ConditionReason = "OperationFailed"
	ReasonOperationInProgress          ConditionReason = "OperationInProgress"
	ReasonOutcomeUnknown               ConditionReason = "OutcomeUnknown"
	ReasonPending                      ConditionReason = "Pending"
	ReasonPlanNoLongerCurrent          ConditionReason = "PlanNoLongerCurrent"
	ReasonPlanPublicationRefused       ConditionReason = "PlanPublicationRefused"
	ReasonPlanReady                    ConditionReason = "PlanReady"
	ReasonPodAdmissionRefused          ConditionReason = "PodAdmissionRefused"
	ReasonPolicyBlocked                ConditionReason = "PolicyBlocked"
	ReasonPolicyChanged                ConditionReason = "PolicyChanged"
	ReasonPolicyRefused                ConditionReason = "PolicyRefused"
	ReasonPolicySatisfied              ConditionReason = "PolicySatisfied"
	ReasonPrivilegeChanges             ConditionReason = "PrivilegeChanges"
	ReasonProofInputsChanged           ConditionReason = "ProofInputsChanged"
	ReasonProtectedTable               ConditionReason = "ProtectedTable"
	ReasonPublished                    ConditionReason = "Published"
	ReasonRealmConflict                ConditionReason = "RealmConflict"
	ReasonRealmNotAuthorized           ConditionReason = "RealmNotAuthorized"
	ReasonRefreshFailed                ConditionReason = "RefreshFailed"
	ReasonRefreshing                   ConditionReason = "Refreshing"
	ReasonRefreshSuspended             ConditionReason = "RefreshSuspended"
	ReasonRequested                    ConditionReason = "Requested"
	ReasonResolveFailed                ConditionReason = "ResolveFailed"
	ReasonRunnerProtocolMismatch       ConditionReason = "RunnerProtocolMismatch"
	ReasonSatisfied                    ConditionReason = "Satisfied"
	// ReasonSnapshotTaken says a ConfigMap source was read and its selected
	// files copied into the snapshot every later operation reads.
	ReasonSnapshotTaken           ConditionReason = "SnapshotTaken"
	ReasonScopedChanges           ConditionReason = "ScopedChanges"
	ReasonScopedConverged         ConditionReason = "ScopedConverged"
	ReasonScopedPlanPending       ConditionReason = "ScopedPlanPending"
	ReasonSourceFreshnessUnknown  ConditionReason = "SourceFreshnessUnknown"
	ReasonSourceRefreshPending    ConditionReason = "SourceRefreshPending"
	ReasonSourceResolutionUnknown ConditionReason = "SourceResolutionUnknown"
	ReasonSourceUnresolved        ConditionReason = "SourceUnresolved"
	ReasonStale                   ConditionReason = "Stale"
	ReasonStaleObservation        ConditionReason = "StaleObservation"
	ReasonStalePlan               ConditionReason = "StalePlan"
	ReasonSucceeded               ConditionReason = "Succeeded"
	ReasonSupersededApproval      ConditionReason = "SupersededApproval"
	ReasonSupportedEngine         ConditionReason = "SupportedEngine"
	ReasonSuspended               ConditionReason = "Suspended"
	ReasonUnsupportedEngine       ConditionReason = "UnsupportedEngine"
	ReasonVerifyingConvergence    ConditionReason = "VerifyingConvergence"
	ReasonWaiting                 ConditionReason = "Waiting"
)

// +kubebuilder:validation:XValidation:rule="oldSelf.hasValue() || self.metadata.name.size() <= 63",message="metadata.name must be at most 63 bytes, because it is carried whole in the labels of the Jobs this resource dispatches",optionalOldSelf=true
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
