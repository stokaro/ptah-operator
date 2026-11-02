package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/ocireference"
)

// SourceKind is the kind of source a resource's spec names.
type SourceKind string

const (
	// SourceOCI is an OCI artifact: spec.desired.ociRef of a schema,
	// spec.artifact.ociRef of a migration.
	SourceOCI SourceKind = "OCI"
	// SourceConfigMap is a ConfigMap in the resource's namespace.
	SourceConfigMap SourceKind = "ConfigMap"
	// SourceGit is one commit of a Git repository.
	SourceGit SourceKind = "Git"
	// SourceOther is a spec that names none of these, such as a kind of
	// source a newer API added.
	SourceOther SourceKind = "Other"
)

// Source is the source a resource's spec names, the settings that come with
// it, and what the operator last resolved it to.
type Source struct {
	// Field is where the spec names it: spec.desired for a schema,
	// spec.artifact for a migration.
	Field string
	Kind  SourceKind
	// Reference is the OCI reference the spec asks for, empty for another
	// kind.
	Reference string
	// Spec is the source as the spec holds it.
	Spec operatorv1alpha1.SourceSpec
	// Apply is spec.policy.apply.
	Apply operatorv1alpha1.ApplyPolicy
	// Resolved and Digest are the artifact the operator last resolved an OCI
	// source to, empty when it has resolved none.
	Resolved string
	Digest   string
	// Verified reports that the verification policy accepted Digest.
	Verified bool
	// PlanDigest is the artifact a schema's current plan was computed from.
	// A migration's status names its plan without one; the plan of a cycle
	// is computed from the artifact that cycle resolved, which is Digest.
	PlanDigest string
}

// SourceOf reads the source of a PtahSchema or a PtahMigration.
func SourceOf(object client.Object) (Source, error) {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		status := &typed.Status
		source := Source{
			Field: "spec.desired", Kind: sourceKind(typed.Spec.Desired), Reference: typed.Spec.Desired.OCIRef,
			Spec: *typed.Spec.Desired.DeepCopy(), Apply: typed.Spec.Policy.Apply,
			Resolved: status.Source.ResolvedReference, Digest: status.Source.Digest, Verified: status.Source.Verified,
		}
		if plan := status.Plan; plan != nil {
			source.PlanDigest = plan.ArtifactDigest
		}
		return source, nil
	case *operatorv1alpha1.PtahMigration:
		source := Source{
			Field: "spec.artifact", Kind: sourceKind(typed.Spec.Artifact), Reference: typed.Spec.Artifact.OCIRef,
			Spec: *typed.Spec.Artifact.DeepCopy(), Apply: typed.Spec.Policy.Apply,
			Verified: meta.IsStatusConditionTrue(typed.Status.Conditions, operatorv1alpha1.ConditionMigrationArtifactVerified),
		}
		if artifact := typed.Status.Artifact; artifact != nil {
			source.Resolved, source.Digest = artifact.ResolvedReference, artifact.Digest
		}
		return source, nil
	default:
		return Source{}, fmt.Errorf("%T is neither a PtahSchema nor a PtahMigration", object)
	}
}

func sourceKind(spec operatorv1alpha1.SourceSpec) SourceKind {
	switch {
	case spec.ConfigMap != nil:
		return SourceConfigMap
	case spec.Git != nil:
		return SourceGit
	case spec.OCIRef != "":
		return SourceOCI
	default:
		return SourceOther
	}
}

// pointer is the JSON Pointer of the source's OCI reference.
func (s Source) pointer() string {
	return "/" + strings.ReplaceAll(s.Field, ".", "/") + "/ociRef"
}

// sameRequest reports a source that asks for what other asks for: the same
// kind, and for an OCI source the same reference.
func (s Source) sameRequest(other Source) bool {
	return s.Kind == other.Kind && s.Reference == other.Reference
}

// PinnedReference is an OCI reference that selects one manifest by its
// SHA-256 digest.
type PinnedReference struct {
	// Reference is the reference as given.
	Reference string
	// Digest is the digest it selects, sha256:<64 lowercase hex>.
	Digest string
}

// NotPinnedError is a value ParsePinnedReference refuses.
type NotPinnedError struct {
	Value string
	// Tag is the tag the value names in place of a digest, when it names
	// one: a value ptah oci resolve can turn into a pinned reference.
	Tag     string
	Problem string
}

func (e *NotPinnedError) Error() string {
	return fmt.Sprintf("%q %s", e.Value, e.Problem)
}

var (
	sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// tagName is a tag as the operator's parser validates one where no
	// digest follows (oras-go's tagRegexp).
	tagName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ParsePinnedReference reads an OCI reference that names its artifact by
// digest alone, as oci://HOST/REPOSITORY@sha256:<64 hex>.
//
// It parses the value the way the operator does (internal/ocireference),
// which is stricter than the CRD's pattern, so a value it accepts is one the
// operator resolves. A tag is refused even beside a digest: the operator's
// parser drops such a tag unread, and a stored tag nothing checks would tell
// a reader of the spec which release runs without anything holding it true.
//
// Beside a digest that parser drops everything from the first ":" to the
// "@" without reading it, so what sits there is named for what it is: a
// tag, or a repository path that is not one, such as another reference
// pasted after the registry. A scheme there is named even where the parser
// refuses the value, since its own message would call it a tag.
func ParsePinnedReference(value string) (PinnedReference, error) {
	written, digest, hasDigest := strings.Cut(repositoryPath(value), "@")
	parsed, err := ocireference.Parse(value)
	if strings.Contains(written, "://") {
		problem := fmt.Sprintf("has %s where its repository path belongs, and a repository path carries no scheme", written)
		if err == nil {
			problem += fmt.Sprintf(": the operator would read repository %s from it and drop the rest unread", parsed.Repository)
		}
		return PinnedReference{}, &NotPinnedError{Value: value, Problem: problem}
	}
	if err != nil {
		return PinnedReference{}, &NotPinnedError{Value: value, Problem: "is not an OCI reference the operator accepts: " + err.Error()}
	}
	_, tag, hasTag := strings.Cut(written, ":")
	switch {
	case !hasDigest && !hasTag:
		return PinnedReference{}, &NotPinnedError{Value: value, Tag: "latest",
			Problem: "names no digest, and no tag either, which means tag latest"}
	case !hasDigest:
		// The parser accepted it, so it is a tag.
		return PinnedReference{}, &NotPinnedError{Value: value, Tag: tag, Problem: fmt.Sprintf("names tag %s, not a digest", tag)}
	case hasTag && !tagName.MatchString(tag):
		return PinnedReference{}, &NotPinnedError{Value: value, Problem: fmt.Sprintf("has %s where its repository path "+
			"belongs, which is not a repository path, with a tag or without: the operator would read repository %s "+
			"from it and drop the rest unread", written, parsed.Repository)}
	case hasTag:
		return PinnedReference{}, &NotPinnedError{Value: value, Problem: fmt.Sprintf("names tag %s beside its digest; "+
			"the operator reads the digest alone, so the tag would be stored and never checked", tag)}
	case !sha256Digest.MatchString(digest):
		return PinnedReference{}, &NotPinnedError{Value: value, Problem: fmt.Sprintf("names digest %s; the operator "+
			"pins an artifact by sha256: and 64 lowercase hexadecimal digits", digest)}
	}
	return PinnedReference{Reference: value, Digest: digest}, nil
}

// repositoryPath is what follows the registry in a reference that parsed:
// the repository and its selector. The registry is everything up to the
// first "/", as the parser splits it, so a port there is not a tag.
func repositoryPath(value string) string {
	_, path, _ := strings.Cut(strings.TrimPrefix(value, "oci://"), "/")
	return path
}

// SourceResult is what SetSource asked for and what the API server
// answered. Generation is the requested state, the one the operator has to
// observe; ObservedGeneration and Phase are what it had observed when the
// answer was written.
type SourceResult struct {
	Ref Ref
	// Reference is the pinned reference the spec now asks for.
	Reference string
	// Previous is the reference the spec asked for before the change; when
	// nothing was sent it is Reference.
	Previous string
	// Changed is false when the spec already asked for Reference and
	// nothing was sent, or nothing sent was stored.
	Changed bool
	// SetByAnother is a change another writer made first: the spec came to
	// ask for Reference between SetSource's read and its patch, which the
	// API server refused. Changed is false.
	SetByAnother       bool
	UID                types.UID
	Generation         int64
	ObservedGeneration int64
	Phase              string
	// Running is the operation in flight, if any. An Apply here is a claim
	// whose Job was not created: SetSource changes no source under an Apply
	// whose Job may exist, and the operator drops this one when it sees the
	// change, so its plan does not run.
	Running string
	// ProofOwed is a schema's post-Apply proof not yet taken. It outranks a
	// newer generation, so the new reference is resolved after it.
	ProofOwed bool
	// Unresolved is a migration with a run nobody accounted for. While the
	// record stands the migration publishes no plan.
	Unresolved bool
	// Suspended reports spec.suspend: a suspended resource starts no Job, so
	// nothing resolves the new reference until it is resumed.
	Suspended bool
	// Source is the source as the answer carried it.
	Source Source
	// Object is the resource as the answer carried it, a baseline for a
	// wait that follows.
	Object client.Object
}

// SourceKindError is a resource whose source is a ConfigMap or Git, which
// SetSource does not change: moving to an OCI artifact also takes a
// verification policy, which only the person editing the resource can name.
type SourceKindError struct {
	Ref   Ref
	Field string
	Kind  SourceKind
}

func (e *SourceKindError) Error() string {
	if e.Kind == SourceOther {
		return fmt.Sprintf("%s of %s names no OCI artifact", e.Field, e.Ref)
	}
	return fmt.Sprintf("%s of %s names a %s source, not an OCI artifact", e.Field, e.Ref, e.Kind)
}

// ApplyRunningError is a resource with an Apply in flight whose Job may
// exist, by the rule the controller decides it with
// (mutationlifecycle.MayHaveDispatched). SetSource does not change its
// source.
//
// A spec change moves the generation, which is one of the inputs an Apply
// claim was decided from. The operator drops a claim whose Job it has not
// created, and records one whose Job may exist as an outcome nobody
// established, without reading the result. For a migration that is a run
// nobody accounted for, and a reading clears that record only once nothing of
// the artifact is left to apply: with a new artifact's versions pending, it
// takes a person's acknowledgment. The Apply cannot take the new reference
// anyway; changing the source after it ends costs nothing.
type ApplyRunningError struct {
	Ref Ref
	Job string
}

func (e *ApplyRunningError) Error() string {
	return fmt.Sprintf("an Apply is running on %s (Job %s)", e.Ref, e.Job)
}

// SourceChangedError is a source another writer changed between SetSource's
// read and its patch. SetSource does not replace it: the reference it would
// overwrite is one the person never saw.
type SourceChangedError struct {
	Ref   Ref
	Field string
	// Was is the reference SetSource read.
	Was string
	// Now is the source the resource names now.
	Now Source
	// sent is the API server's refusal of the patch that met the change.
	sent error
}

func (e *SourceChangedError) Error() string {
	return fmt.Sprintf("%s of %s changed after it was read: it asked for %s and now asks for %s",
		e.Field, e.Ref, e.Was, describeRequest(e.Now))
}

// Unwrap is the refusal, so Classify reads the change as refused: nothing was
// stored.
func (e *SourceChangedError) Unwrap() error { return e.sent }

func describeRequest(s Source) string {
	switch s.Kind {
	case SourceOCI:
		return s.Reference
	case SourceOther:
		return "a source that is not an OCI artifact"
	default:
		return "a " + string(s.Kind) + " source"
	}
}

// SetSource points the resource ref names at another OCI artifact: it sets
// spec.desired.ociRef of a schema or spec.artifact.ociRef of a migration to
// pinned and changes nothing else. The verification policy, the registry
// credential, the transport and the apply policy stay as they are, and the
// new artifact is held to them.
//
// It sends nothing when the spec already asks for pinned. It refuses a
// resource whose source is a ConfigMap or Git (*SourceKindError) and one with
// an Apply in flight whose Job may exist (*ApplyRunningError).
//
// The patch tests metadata.uid, as every change here does, and carries the
// resourceVersion of the read as well, which SetSuspend and Refresh do not.
// Both of those decide from nothing but the field they write, so another
// writer's change between the read and the patch takes nothing from them,
// and failing on the operator's own status writes would only make them retry.
// SetSource decides from the spec it read -- the kind of source and the
// reference it replaces -- and from status, where an Apply claim is, so its
// patch holds only while that document is still the stored one. A conflict
// is read again: another writer's reference, or another kind of source, is
// returned as *SourceChangedError rather than overwritten, a claimed Apply as
// *ApplyRunningError, and anything else is decided again from the fresh
// read, three times at most.
func SetSource(ctx context.Context, c client.Client, ref Ref, pinned PinnedReference) (SourceResult, error) {
	const attempts = 3
	var (
		first    *Source
		firstUID types.UID
		lastErr  error
	)
	for range attempts {
		object, err := ref.newObject()
		if err != nil {
			return SourceResult{}, err
		}
		if err := c.Get(ctx, ref.key(), object); err != nil {
			if lastErr != nil {
				return SourceResult{}, &rereadError{refused: lastErr, read: ref.failed("read", err)}
			}
			return SourceResult{}, ref.failed("read", err)
		}
		read, err := snapshotOf(object)
		if err != nil {
			return SourceResult{}, err
		}
		source, err := SourceOf(object)
		if err != nil {
			return SourceResult{}, err
		}
		switch {
		case first == nil:
			first, firstUID = &source, read.uid
		case read.uid != firstUID:
			return SourceResult{}, ref.failed("change", ErrReplaced)
		case source.Kind == SourceOCI && source.Reference == pinned.Reference:
			// What was asked for holds, by another writer's hand.
			result := sourceResult(ref, first.Reference, false, read, source)
			result.SetByAnother = true
			return result, nil
		case !source.sameRequest(*first):
			return SourceResult{}, &SourceChangedError{Ref: ref, Field: source.Field, Was: first.Reference, Now: source, sent: lastErr}
		}
		if source.Kind != SourceOCI {
			return SourceResult{}, &SourceKindError{Ref: ref, Field: source.Field, Kind: source.Kind}
		}
		if source.Reference == pinned.Reference {
			return sourceResult(ref, source.Reference, false, read, source), nil
		}
		if applying(read) && read.dispatched {
			return SourceResult{}, &ApplyRunningError{Ref: ref, Job: runningJob(object)}
		}
		patch, err := json.Marshal([]patchOperation{
			{Op: "test", Path: "/metadata/uid", Value: read.uid},
			{Op: "replace", Path: "/metadata/resourceVersion", Value: object.GetResourceVersion()},
			{Op: "replace", Path: source.pointer(), Value: pinned.Reference},
		})
		if err != nil {
			return SourceResult{}, err
		}
		err = c.Patch(ctx, object, client.RawPatch(types.JSONPatchType, patch), client.FieldOwner(FieldManager))
		if err == nil {
			written, err := snapshotOf(object)
			if err != nil {
				return SourceResult{}, err
			}
			answered, err := SourceOf(object)
			if err != nil {
				return SourceResult{}, err
			}
			return sourceResult(ref, source.Reference, true, written, answered), nil
		}
		lastErr = classifyPatchError(ctx, c, ref, read.uid, nil, err)
		if !apierrors.IsConflict(err) || errors.Is(lastErr, ErrReplaced) || apierrors.IsNotFound(lastErr) {
			return SourceResult{}, lastErr
		}
	}
	return SourceResult{}, lastErr
}

// rereadError is a change the API server refused with a conflict, after which
// the read that would have decided again failed. The refusal is the answer
// to the change: nothing was stored, and Classify reads it as refused. A
// resource found gone on that read is absent instead.
type rereadError struct {
	refused error
	read    error
}

func (e *rereadError) Error() string {
	return fmt.Sprintf("%v; reading it again after that conflict failed: %v", e.refused, e.read)
}

// Unwrap puts the answer Classify should find first in front.
func (e *rereadError) Unwrap() []error {
	if apierrors.IsNotFound(e.read) {
		return []error{e.read, e.refused}
	}
	return []error{e.refused, e.read}
}

func sourceResult(ref Ref, previous string, changed bool, s snapshot, source Source) SourceResult {
	result := SourceResult{
		Ref: ref, Reference: source.Reference, Previous: previous, Changed: changed, UID: s.uid,
		Generation: s.generation, ObservedGeneration: s.observedGeneration, Phase: s.phase,
		Running: s.operation, ProofOwed: s.proof, Source: source, Object: s.object,
	}
	result.Unresolved = s.unresolved != nil
	result.Suspended = s.suspend
	return result
}

// applying reports an Apply claim in flight, in either family, whether or
// not its Job was created.
func applying(s snapshot) bool {
	return s.operation == string(operatorv1alpha1.OperationApply) ||
		s.operation == string(operatorv1alpha1.MigrationOperationApply)
}

func runningJob(object client.Object) string {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		if operation := typed.Status.ActiveOperation; operation != nil {
			return operation.JobName
		}
	case *operatorv1alpha1.PtahMigration:
		if operation := typed.Status.ActiveOperation; operation != nil {
			return operation.JobName
		}
	}
	return ""
}

// TargetPlanned is the end of a change of source: a plan from the new
// reference waits for an approval, or the resource rests in sync from it,
// whichever the operator reaches. It is waited on only with
// WaitOptions.Source; a command line names approval or in-sync, and a
// Result names the one that was met.
const TargetPlanned Target = "approval or in-sync"

const (
	// StopSourceChanged is a wait for the source SetSource wrote on a
	// resource whose spec now names another one.
	StopSourceChanged StopReason = "SourceChanged"
	// StopNothingPlanned is a wait for an approval of a plan from the new
	// reference on a resource that rests in sync from it: the operator read
	// the database after the change and found nothing to do, so no plan from
	// that change will wait.
	StopNothingPlanned StopReason = "NothingPlanned"
)

// checkSource decides a wait for the reference SetSource wrote. The reading
// is looked at once per document, so the bookkeeping of which reading is
// fresh sees each one in order, and both endings are decided from that one
// look.
func (w *waiter) checkSource(s snapshot) verdict {
	fresh := w.fresh.see(s)
	if held, ok := w.policyHeld(s, fresh); ok {
		return held
	}
	approval := w.approval(s, fresh)
	if approval.done {
		approval = w.sourceReached(s, TargetApproval)
	}
	inSync := w.inSync(s, fresh)
	if inSync.done {
		inSync = w.sourceReached(s, TargetInSync)
	}
	switch w.options.Target {
	case TargetApproval:
		if inSync.done {
			return w.stop(s, StopNothingPlanned, fmt.Sprintf("the operator read the database after the change and "+
				"found nothing %s asks for missing: the resource rests in sync, and no plan waits", w.options.Source))
		}
		return approval
	case TargetInSync:
		return inSync
	}
	switch {
	case approval.done:
		approval.reached = TargetApproval
		return approval
	case inSync.done:
		inSync.reached = TargetInSync
		return inSync
	}
	return waitingFor("a plan waiting for an approval (%s) or the resource in sync (%s)", approval.waiting, inSync.waiting)
}

// sourceMoved stops a wait whose resource no longer asks for the reference
// the wait is for: whatever it plans from now is another writer's artifact.
func (w *waiter) sourceMoved(s snapshot) (verdict, bool) {
	source, err := SourceOf(s.object)
	if err != nil || (source.Kind == SourceOCI && source.Reference == w.options.Source) {
		return verdict{}, false
	}
	return w.stop(s, StopSourceChanged, fmt.Sprintf("%s now asks for %s, not %s: another change replaced "+
		"the reference this wait is for", source.Field, describeRequest(source), w.options.Source)), true
}

// sourceReached holds a document that reached the target to the reference
// the wait is for: a schema's plan computed from its digest, and otherwise
// the artifact the operator resolved. With the generation and a fresh
// reading this is redundant while one writer edits the spec, and it is what
// still holds when another set a reference in between and then set this one
// back.
func (w *waiter) sourceReached(s snapshot, target Target) verdict {
	source, err := SourceOf(s.object)
	if err != nil {
		return waitingFor("a resource of a known kind")
	}
	if target == TargetApproval && s.family == FamilySchema {
		if source.PlanDigest != w.sourceDigest {
			return waitingFor("a plan computed from %s (the current plan is from %s)", w.sourceDigest, orNone(source.PlanDigest))
		}
		return verdict{done: true}
	}
	if source.Digest != w.sourceDigest {
		return waitingFor("the operator to resolve %s (status records %s)", w.options.Source, orNone(source.Digest))
	}
	return verdict{done: true}
}

func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
