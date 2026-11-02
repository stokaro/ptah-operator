package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

var (
	oldDigest = "sha256:" + strings.Repeat("a", 64)
	newDigest = "sha256:" + strings.Repeat("b", 64)
	oldRef    = "oci://registry.example/shop/storefront@" + oldDigest
	newRef    = "oci://registry.example/shop/storefront@" + newDigest
)

// onArtifact is a resource of family at rest in sync on oldRef, with a
// verification policy and a registry credential the change has to keep.
func onArtifact(family string, apply operatorv1alpha1.ApplyPolicy) client.Object {
	object := resting(family)
	source := operatorv1alpha1.SourceSpec{
		OCIRef: oldRef,
		VerificationPolicyFrom: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "ptah-verification-policy"}, Key: "policy.yaml"},
		RegistryAuthFrom: &operatorv1alpha1.RegistryAuthSource{Name: "regcred"},
	}
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Spec.Desired, typed.Spec.Policy.Apply = source, apply
		typed.Status.Source = operatorv1alpha1.SchemaSourceStatus{RequestedReference: oldRef, ResolvedReference: oldRef,
			Digest: oldDigest, Verified: true}
	case *operatorv1alpha1.PtahMigration:
		typed.Spec.Artifact, typed.Spec.Policy.Apply = source, apply
		typed.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{ResolvedReference: oldRef, Digest: oldDigest}
		typed.Status.Conditions = append(typed.Status.Conditions, metav1.Condition{
			Type: operatorv1alpha1.ConditionMigrationArtifactVerified, Status: metav1.ConditionTrue, Reason: "PolicySatisfied",
			Message: "The resolved artifact satisfied its verification policy", ObservedGeneration: 4})
	}
	return object
}

func referenceOn(t *testing.T, api *cluster, family string) string {
	t.Helper()
	switch typed := stored(t, api, family).(type) {
	case *operatorv1alpha1.PtahSchema:
		return typed.Spec.Desired.OCIRef
	case *operatorv1alpha1.PtahMigration:
		return typed.Spec.Artifact.OCIRef
	}
	return ""
}

func TestSetSourceSetsTheReferenceAndReportsWhatTheOperatorObserved(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			api := newCluster(t, onArtifact(family, operatorv1alpha1.ApplyPolicyOnApproval))
			field, reads := "spec.desired.ociRef", "reads the database and plans from what it finds"
			if family == "migration" {
				field, reads = "spec.artifact.ociRef", "reads the migration history and plans the versions still pending"
			}

			code, stdout, stderr := runCommand(t, api, "set-source", family+"/storefront", "--oci", newRef)

			if code != exitOK || api.patchCount() != 1 || referenceOn(t, api, family) != newRef {
				t.Fatalf("set-source = %d after %d patches, reference %s; stderr %q", code, api.patchCount(), referenceOn(t, api, family), stderr)
			}
			requireSays(t, "set-source's report", stdout,
				"Source set for ptah"+family+"/storefront in namespace demo.",
				"Requested: "+field+" "+newRef+", generation 4",
				"Replaced:  "+oldRef,
				"Kept:      verificationPolicyFrom ptah-verification-policy key policy.yaml, registryAuthFrom regcred",
				"Observed:  generation 4, phase InSync",
				"Resolved:  "+oldRef+", verified",
				"Apply:     spec.policy.apply OnApproval",
				"Once the operator observes generation 4 it resolves the new reference, checks it against the verification policy, "+reads+".",
				"A plan or an approval from the previous artifact does not carry over.",
				"Under spec.policy.apply OnApproval that plan waits for an approval: kubectl ptah approve "+family+"/storefront -n demo reviews it.",
				"set-source --wait waits in the same command until a plan from the new reference waits for an approval")
			if labels := stored(t, api, family).GetLabels(); labels["team"] != "shop" {
				t.Fatalf("set-source changed the labels to %v", labels)
			}

			code, stdout, _ = runCommand(t, api, "set-source", "ptah"+family+"/storefront", "--oci", newRef)

			if code != exitOK || api.patchCount() != 1 {
				t.Fatalf("set-source again = %d after %d patches, want nothing sent", code, api.patchCount())
			}
			requireSays(t, "the second report", stdout,
				field+" of ptah"+family+"/storefront in namespace demo already holds that reference; nothing was sent.")
			if strings.Contains(stdout, "Replaced:") {
				t.Fatalf("a change nothing sent names a reference it replaced: %q", stdout)
			}
		})
	}
}

// What the report says the new plan may do follows the resource's apply
// policy, in the terms the API gives it.
func TestSetSourceSaysWhatTheApplyPolicyLetsThePlanDo(t *testing.T) {
	for _, test := range []struct {
		family string
		policy operatorv1alpha1.ApplyPolicy
		says   string
	}{
		{"schema", operatorv1alpha1.ApplyPolicyAlways,
			"Under spec.policy.apply Always the operator applies that plan without a person, unless it is destructive or changes privileges: those wait for an approval."},
		{"migration", operatorv1alpha1.ApplyPolicyAlways,
			"Under spec.policy.apply Always that plan runs as soon as it is published, without a person."},
		{"schema", operatorv1alpha1.ApplyPolicyNever, "Under spec.policy.apply Never the plan is recorded and nothing applies it."},
	} {
		t.Run(test.family+"/"+string(test.policy), func(t *testing.T) {
			api := newCluster(t, onArtifact(test.family, test.policy))

			code, stdout, stderr := runCommand(t, api, "set-source", test.family+"/storefront", "--oci", newRef)

			if code != exitOK {
				t.Fatalf("set-source = %d, stderr %q", code, stderr)
			}
			requireSays(t, "set-source's report", stdout, test.says)
			if strings.Contains(stdout, "kubectl ptah approve") {
				t.Fatalf("the report under %s sends the person to approve: %q", test.policy, stdout)
			}
		})
	}
}

// A reference set-source cannot pin is refused before the cluster is
// reached, with the commands that print one.
func TestSetSourceRefusesAReferenceItCannotPin(t *testing.T) {
	previous := connector
	connector = func(string, string, string, time.Duration) (client.Client, string, error) {
		t.Error("set-source reached for the cluster with a reference it refuses")
		return nil, "", apierrors.NewServiceUnavailable("no cluster here")
	}
	t.Cleanup(func() { connector = previous })

	for _, test := range []struct {
		name      string
		arguments []string
		says      []string
	}{
		{"no reference", []string{"set-source", "schema/storefront"},
			[]string{"set-source needs --oci", "ptah schema push --format reference", "ptah oci resolve <reference>"}},
		{"a tag", []string{"set-source", "migration/orders", "--oci", "oci://registry.example/shop/orders:1.5.0"},
			[]string{`names tag 1.5.0, not a digest`, "ptah migrations push --format reference",
				"ptah oci resolve oci://registry.example/shop/orders:1.5.0 the one a tag names now", "Nothing was sent."}},
		{"no selector", []string{"set-source", "schema/storefront", "--oci", "oci://registry.example/shop/storefront"},
			[]string{"which means tag latest", "ptah oci resolve oci://registry.example/shop/storefront"}},
		{"a tag beside the digest", []string{"set-source", "schema/storefront", "--oci", "oci://registry.example/shop/storefront:1.5.0@" + newDigest},
			[]string{"names tag 1.5.0 beside its digest", "ptah oci resolve <reference>"}},
		{"no scheme", []string{"set-source", "schema/storefront", "--oci", "registry.example/shop/storefront@" + newDigest},
			[]string{"oci:// scheme", "ptah schema push --format reference"}},
		{"another algorithm", []string{"set-source", "schema/storefront", "--oci", "oci://registry.example/shop/storefront@sha512:" + strings.Repeat("b", 128)},
			[]string{"names digest sha512:"}},
		{"a target set-source does not wait for", []string{"set-source", "schema/storefront", "--oci", newRef, "--for", "applied"},
			[]string{"set-source waits for in-sync or approval, not applied"}},
		{"a wait with no time", []string{"set-source", "schema/storefront", "--oci", newRef, "--wait", "--wait-timeout", "0s"},
			[]string{"--wait-timeout has to be longer than zero"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(context.Background(), test.arguments, &stdout, &stderr)

			if code != exitUsage || stdout.Len() != 0 {
				t.Fatalf("run(%q) = %d, stdout %q, stderr %q; want usage", test.arguments, code, stdout.String(), stderr.String())
			}
			requireSays(t, "the refusal", stderr.String(), test.says...)
		})
	}
}

func TestSetSourceRefusesWhatItDoesNotChange(t *testing.T) {
	configMap := onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval).(*operatorv1alpha1.PtahSchema)
	configMap.Spec.Desired = operatorv1alpha1.SourceSpec{ConfigMap: &operatorv1alpha1.ConfigMapSourceSpec{Name: "storefront-schema",
		Items: []operatorv1alpha1.ConfigMapSourceItem{{Key: "schema.sql", Path: "schema.sql"}}}}
	git := onArtifact("migration", operatorv1alpha1.ApplyPolicyOnApproval).(*operatorv1alpha1.PtahMigration)
	git.Spec.Artifact = operatorv1alpha1.SourceSpec{Git: &operatorv1alpha1.GitSourceSpec{URL: "https://git.example/shop/orders",
		Ref: operatorv1alpha1.GitRef{Branch: "main"}, Path: "."}}
	applying := onArtifact("migration", operatorv1alpha1.ApplyPolicyOnApproval).(*operatorv1alpha1.PtahMigration)
	applying.Status.Phase = operatorv1alpha1.MigrationPhaseApplying
	applying.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{Type: "Apply", ID: "attempt", JobName: "ptah-apply-1",
		Attempt: 1, DispatchStarted: true}
	applyingSchema := onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval).(*operatorv1alpha1.PtahSchema)
	applyingSchema.Status.Phase = operatorv1alpha1.PhaseApplying
	applyingSchema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: "Apply", ID: "attempt", JobName: "ptah-apply-1",
		JobUID: "uid-job"}

	for _, test := range []struct {
		name   string
		family string
		object client.Object
		says   []string
	}{
		{"a ConfigMap source", "schema", configMap, []string{
			"spec.desired of ptahschema/storefront in namespace demo names a ConfigMap source, not an OCI artifact. Nothing was changed.",
			"spec.desired.verificationPolicyFrom", "  kubectl edit ptahschema storefront -n demo"}},
		{"a Git source", "migration", git, []string{
			"spec.artifact of ptahmigration/storefront in namespace demo names a Git source", "  kubectl edit ptahmigration storefront -n demo"}},
		{"a migration Apply running", "migration", applying, []string{
			"an Apply is running on ptahmigration/storefront in namespace demo (Job ptah-apply-1). Nothing was changed.",
			"records the run as one nobody accounted for (Unknown)", "kubectl ptah wait migration/storefront --for applied -n demo waits for it"}},
		{"a schema Apply running", "schema", applyingSchema, []string{
			"an Apply is running on ptahschema/storefront", "records its outcome as unknown without reading its result"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newCluster(t, test.object)

			code, stdout, stderr := runCommand(t, api, "set-source", test.family+"/storefront", "--oci", newRef)

			if code != exitNotDone || stdout != "" || api.patchCount() != 0 {
				t.Fatalf("set-source = %d after %d patches, stdout %q, stderr %q", code, api.patchCount(), stdout, stderr)
			}
			requireSays(t, "set-source's stderr", stderr, test.says...)
		})
	}
}

// An Apply claimed with no Job yet is dropped by the change, and the report
// says its plan does not run.
func TestSetSourceSaysAnApplyWithNoJobIsDropped(t *testing.T) {
	claimed := onArtifact("schema", operatorv1alpha1.ApplyPolicyAlways).(*operatorv1alpha1.PtahSchema)
	claimed.Status.Phase = operatorv1alpha1.PhaseApplying
	claimed.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: "Apply", ID: "attempt", JobName: "ptah-apply-1"}
	api := newCluster(t, claimed)

	code, stdout, stderr := runCommand(t, api, "set-source", "schema/storefront", "--oci", newRef)

	if code != exitOK || api.patchCount() != 1 {
		t.Fatalf("set-source = %d after %d patches, stderr %q", code, api.patchCount(), stderr)
	}
	requireSays(t, "set-source's report", stdout, "Running:   Apply operation",
		"The Apply claimed from the previous spec has no Job yet: the operator drops it when it sees the change, and its plan does not run.")
}

// unreadAfterConflict is the operator writing status between the command's
// read and its patch, and the API server then answering no read.
type unreadAfterConflict struct{ *cluster }

func (u unreadAfterConflict) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	stored := &operatorv1alpha1.PtahSchema{}
	if err := u.cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), stored); err != nil {
		return err
	}
	stored.Status.Phase = operatorv1alpha1.PhaseResolving
	if err := u.cluster.Client.Status().Update(ctx, stored); err != nil {
		return err
	}
	err := u.cluster.Patch(ctx, object, patch, options...)
	u.cluster.mu.Lock()
	u.cluster.readErr = apierrors.NewServiceUnavailable("etcd is not ready")
	u.cluster.mu.Unlock()
	return err
}

// A conflict whose read again fails is a refused change: exit 5, and the
// message names the conflict and the read.
func TestSetSourceSaysAConflictWasRefusedWhenItCannotReadAgain(t *testing.T) {
	api := newCluster(t, onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval))

	code, stdout, stderr := runCommand(t, unreadAfterConflict{api}, "set-source", "schema/storefront", "--oci", newRef)

	if code != exitRefused || stdout != "" {
		t.Fatalf("set-source = %d, stdout %q, stderr %q; want %d", code, stdout, stderr, exitRefused)
	}
	requireSays(t, "set-source's stderr", stderr, "Operation cannot be fulfilled",
		"reading it again after that conflict failed", "etcd is not ready", "Nothing was changed.")
	api.readErr = nil
	if got := referenceOn(t, api, "schema"); got != oldRef {
		t.Fatalf("the reference is %s, want %s", got, oldRef)
	}
}

func TestSetSourceExitsByWhatTheAPIServerSaid(t *testing.T) {
	forbidden := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
		Reason:  metav1.StatusReasonForbidden,
		Message: `ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob" cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "demo"`}}
	for _, test := range []struct {
		name   string
		refuse error
		code   int
		says   []string
	}{
		{"RBAC refuses the patch", forbidden, exitRefused,
			[]string{`User "bob" cannot patch`, "Nothing was changed. set-source needs get and patch on ptahschemas in namespace demo."}},
		{"the patch has no answer", context.DeadlineExceeded, exitUnknown,
			[]string{"no answer said whether it was stored", "Running it again sends nothing when the spec already holds the reference."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newCluster(t, onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval))
			api.refuse = test.refuse

			code, stdout, stderr := runCommand(t, api, "set-source", "schema/storefront", "--oci", newRef)

			if code != test.code || stdout != "" {
				t.Fatalf("set-source = %d, stdout %q, stderr %q; want %d", code, stdout, stderr, test.code)
			}
			requireSays(t, "set-source's stderr", stderr, test.says...)
		})
	}
}

// bumping is the API server moving metadata.generation on a spec patch, which
// the fake one does not do.
type bumping struct{ *cluster }

func (b bumping) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if err := b.cluster.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	object.SetGeneration(object.GetGeneration() + 1)
	if err := b.cluster.Client.Update(ctx, object); err != nil {
		return err
	}
	return b.cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), object)
}

// awaiting is the operator's answer to generation 5 of a schema waiting for
// an approval of a plan computed from planned, from a reading at
// readAgainAt, with the new reference resolved.
func awaiting(planned string) func(client.Object) {
	return schemaStatus(func(status *operatorv1alpha1.PtahSchemaStatus) {
		at := metav1.NewTime(readAgainAt)
		status.ObservedGeneration, status.Phase, status.Target.LastObservedAt = 5, operatorv1alpha1.PhaseAwaitingApproval, &at
		status.Source = operatorv1alpha1.SchemaSourceStatus{RequestedReference: newRef, ResolvedReference: newRef, Digest: newDigest, Verified: true}
		status.Plan = &operatorv1alpha1.CurrentPlanStatus{Name: "ptah-plan-" + planned[7:10], UID: "uid-plan", Fingerprint: "sha256:fp", ArtifactDigest: planned}
		status.Conditions = []metav1.Condition{
			{Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue, Reason: "AwaitingApproval",
				Message: "The plan needs an approval", ObservedGeneration: 5},
			{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "AwaitingApproval",
				Message: "Plan is waiting for approval", ObservedGeneration: 5},
		}
	})
}

// set-source --for approval waits for a plan from the new reference. A plan
// from another artifact at the change's generation, from a fresh reading, is
// one a wait on the generation alone takes; the operator leaves that shape
// when another writer points the resource elsewhere and back before it acts,
// and it is not the plan this change asked for.
func TestSetSourceWaitsForAPlanFromTheNewReference(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval))
		api.then(unchanged, awaiting(oldDigest), awaiting(newDigest))

		code, stdout, stderr := runCommand(t, bumping{api}, "set-source", "schema/storefront", "--oci", newRef, "--for", "approval")

		if code != exitOK {
			t.Fatalf("set-source --for approval = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "set-source --for approval's report", stdout, "Source set for", "generation 5",
			"ptahschema/storefront in namespace demo reached approval.", "Plan:       ptah-plan-bbb, fingerprint sha256:fp",
			"kubectl ptah approve schema/storefront -n demo reviews it.")
		if strings.Contains(stdout, "--for in-sync until") {
			t.Fatalf("set-source --for approval points at a wait it already ran: %q", stdout)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval))
		api.then(unchanged, awaiting(oldDigest))

		code, stdout, stderr := runCommand(t, bumping{api}, "set-source", "schema/storefront", "--oci", newRef,
			"--for", "approval", "--wait-timeout", "1m")

		if code != exitTimedOut || strings.Contains(stdout, "reached") {
			t.Fatalf("set-source --for approval on the previous artifact's plan = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "the timeout", stderr, "did not reach approval within 1m0s.", "Phase:       AwaitingApproval",
			"Waiting for: a plan computed from "+newDigest+" (the current plan is from "+oldDigest+")")
	})
}

// A wait after a change nothing sent begins after that change, so it counts
// the next reading of the database and says so.
func TestSetSourceWaitsForTheNextReadingWhenNothingWasSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolved := onArtifact("migration", operatorv1alpha1.ApplyPolicyOnApproval).(*operatorv1alpha1.PtahMigration)
		resolved.Spec.Artifact.OCIRef = newRef
		resolved.Status.Artifact = &operatorv1alpha1.SourceAccessBinding{ResolvedReference: newRef, Digest: newDigest}
		api := newCluster(t, resolved)
		api.armed = true
		api.then(unchanged, unchanged, readAgain)

		code, stdout, stderr := runCommand(t, api, "set-source", "migration/storefront", "--oci", newRef, "--wait")

		if code != exitOK || api.patchCount() != 0 {
			t.Fatalf("set-source --wait = %d after %d patches, stdout %q, stderr %q", code, api.patchCount(), stdout, stderr)
		}
		requireSays(t, "set-source --wait's report", stdout, "already holds that reference; nothing was sent.",
			"Nothing was sent, so the wait counts only a reading that begins after it",
			"kubectl ptah refresh migration/storefront -n demo asks for one now.",
			"reached in-sync.", "Database:   last read at 2026-10-01T09:07:00Z")
	})
}

// --wait alone ends on whichever the operator reaches from the new
// reference: under OnApproval, the plan waiting for a person.
func TestSetSourceWaitEndsOnAPlanUnderOnApproval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval))
		api.then(unchanged, awaiting(newDigest))

		code, stdout, stderr := runCommand(t, bumping{api}, "set-source", "schema/storefront", "--oci", newRef, "--wait")

		if code != exitOK {
			t.Fatalf("set-source --wait = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "set-source --wait's report", stdout, "reached approval.", "Plan:       ptah-plan-bbb")
		requireSays(t, "set-source --wait's stderr", stderr, "to reach approval or in-sync.")
	})
}

// --for approval on an artifact that asks for nothing the database lacks
// stops once the operator rests in sync from it: no plan will wait.
func TestSetSourceWaitForApprovalStopsWhenNothingIsPlanned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, onArtifact("migration", operatorv1alpha1.ApplyPolicyOnApproval))
		inSync := migrationStatus(func(status *operatorv1alpha1.PtahMigrationStatus) {
			at := metav1.NewTime(readAgainAt)
			status.ObservedGeneration, status.History.ObservedAt = 5, at
			status.Artifact = &operatorv1alpha1.SourceAccessBinding{ResolvedReference: newRef, Digest: newDigest}
			for index := range status.Conditions {
				status.Conditions[index].ObservedGeneration = 5
			}
		})
		api.then(unchanged, inSync)

		code, stdout, stderr := runCommand(t, bumping{api}, "set-source", "migration/storefront", "--oci", newRef, "--for", "approval")

		if code != exitWillNotReach || strings.Contains(stdout, "reached") {
			t.Fatalf("set-source --for approval on an artifact with nothing to plan = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "the stop", stderr, "will not reach approval: NothingPlanned.", "rests in sync, and no plan waits")
	})
}

// racing is another writer who sets the reference to reference between the
// command's read and its patch.
type racing struct {
	*cluster
	reference string
}

func (r racing) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	stored := &operatorv1alpha1.PtahSchema{}
	if err := r.cluster.Client.Get(ctx, client.ObjectKeyFromObject(object), stored); err != nil {
		return err
	}
	stored.Spec.Desired.OCIRef = r.reference
	if err := r.cluster.Client.Update(ctx, stored); err != nil {
		return err
	}
	return r.cluster.Patch(ctx, object, patch, options...)
}

// Another writer setting the reference asked for first leaves it set: the
// command exits 0 and says whose change it was.
func TestSetSourceSaysWhenAnotherWriterSetTheReferenceFirst(t *testing.T) {
	api := newCluster(t, onArtifact("schema", operatorv1alpha1.ApplyPolicyOnApproval))

	code, stdout, stderr := runCommand(t, racing{api, newRef}, "set-source", "schema/storefront", "--oci", newRef)

	if code != exitOK || referenceOn(t, api, "schema") != newRef {
		t.Fatalf("set-source = %d, reference %s, stderr %q", code, referenceOn(t, api, "schema"), stderr)
	}
	requireSays(t, "set-source's report", stdout, "spec.desired.ociRef of ptahschema/storefront in namespace demo holds that "+
		"reference: another writer set it after this command read "+oldRef+", and the API server refused this command's own change.")
	if strings.Contains(stdout, "Replaced:") || strings.Contains(stdout, "nothing was sent") {
		t.Fatalf("the report claims a change it did not make, or that it sent nothing: %q", stdout)
	}
}

// With -o json stdout carries the resource as the API server answered the
// change, and nothing else.
func TestSetSourcePrintsTheResourceTheAnswerCarried(t *testing.T) {
	api := newCluster(t, onArtifact("migration", operatorv1alpha1.ApplyPolicyOnApproval))

	code, stdout, stderr := runCommand(t, api, "set-source", "migration/storefront", "--oci", newRef, "-o", "json")

	var migration operatorv1alpha1.PtahMigration
	if code != exitOK || yaml.UnmarshalStrict([]byte(stdout), &migration) != nil {
		t.Fatalf("set-source -o json = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if migration.Kind != "PtahMigration" || migration.Spec.Artifact.OCIRef != newRef || stderr != "" {
		t.Fatalf("set-source -o json printed %s with %s, stderr %q", migration.Kind, migration.Spec.Artifact.OCIRef, stderr)
	}
}
