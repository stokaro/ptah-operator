package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

const demo = "demo"

// observedAt is when the fixtures' operator last read the database, and
// readAgainAt a reading after it. Both are far from the clock a test runs
// on, so a wait that compared its own clock with them would show it.
var (
	observedAt  = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	readAgainAt = time.Date(2026, 10, 1, 9, 7, 0, 0, time.UTC)
)

// resting is a resource of family that rests in sync at generation 4, last
// read at observedAt.
func resting(family string) client.Object {
	meta := metav1.ObjectMeta{Namespace: demo, Name: "storefront", UID: "uid-storefront", Generation: 4,
		Labels: map[string]string{"team": "shop"}}
	read := metav1.NewTime(observedAt)
	if family == "migration" {
		return &operatorv1alpha1.PtahMigration{ObjectMeta: meta, Status: operatorv1alpha1.PtahMigrationStatus{
			ObservedGeneration: 4, Phase: operatorv1alpha1.MigrationPhaseInSync,
			History: &operatorv1alpha1.MigrationHistoryStatus{ObservedAt: read, ContractVersion: 1},
			Conditions: []metav1.Condition{{Type: operatorv1alpha1.ConditionMigrationReady, Status: metav1.ConditionTrue,
				Reason: "HistoryMatched", Message: "The database has every migration this artifact carries", ObservedGeneration: 4}},
		}}
	}
	return &operatorv1alpha1.PtahSchema{ObjectMeta: meta, Status: operatorv1alpha1.PtahSchemaStatus{
		ObservedGeneration: 4, Phase: operatorv1alpha1.PhaseInSync,
		Target: operatorv1alpha1.TargetStatus{LastObservedAt: &read},
		Conditions: []metav1.Condition{
			{Type: operatorv1alpha1.ConditionInSync, Status: metav1.ConditionTrue, Reason: "ScopedConverged",
				Message: "A stable scoped plan proves convergence", ObservedGeneration: 4},
			{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "InSync",
				Message: "Schema is in sync", ObservedGeneration: 4},
		},
	}}
}

// cluster is the API server with the operator writing status between the
// plugin's reads. Once armed -- by the first patch or create, or by the test
// -- each read of the resource first applies the next step to what is
// stored, the way the controller's status writes land between two polls.
type cluster struct {
	client.Client
	t *testing.T

	mu       sync.Mutex
	armed    bool
	steps    []func(client.Object)
	patches  int
	creates  int
	refuse   error
	readErr  error
	replaced bool
}

func newCluster(t *testing.T, objects ...client.Object) *cluster {
	t.Helper()
	api := fake.NewClientBuilder().WithScheme(planreadtest.Scheme(t)).
		WithStatusSubresource(&operatorv1alpha1.PtahSchema{}, &operatorv1alpha1.PtahMigration{}).
		WithObjects(objects...).Build()
	return &cluster{Client: api, t: t}
}

func (c *cluster) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	c.mu.Lock()
	err := c.readErr
	var step func(client.Object)
	if c.armed && len(c.steps) > 0 && isResource(object) {
		step, c.steps = c.steps[0], c.steps[1:]
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if step != nil {
		stored := object.DeepCopyObject().(client.Object)
		if err := c.Client.Get(ctx, key, stored); err != nil {
			c.t.Fatalf("read the resource to change its status: %v", err)
		}
		step(stored)
		if err := c.Client.Status().Update(ctx, stored); err != nil {
			c.t.Fatalf("write the status the operator would: %v", err)
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *cluster) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	c.mu.Lock()
	c.patches++
	c.armed = true
	refuse, replace := c.refuse, c.replaced
	c.mu.Unlock()
	if replace {
		// Deleted and created again under the name before the patch arrives.
		stored := object.DeepCopyObject().(client.Object)
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), stored); err != nil {
			return err
		}
		if err := c.Client.Delete(ctx, stored); err != nil {
			return err
		}
		stored.SetResourceVersion("")
		stored.SetUID("uid-another")
		if err := c.Client.Create(ctx, stored); err != nil {
			return err
		}
	}
	if refuse != nil {
		return refuse
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func (c *cluster) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	c.mu.Lock()
	c.creates++
	c.armed = true
	c.mu.Unlock()
	return c.Client.Create(ctx, object, options...)
}

func (c *cluster) then(steps ...func(client.Object)) *cluster {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, steps...)
	return c
}

func (c *cluster) patchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patches
}

func isResource(object client.Object) bool {
	switch object.(type) {
	case *operatorv1alpha1.PtahSchema, *operatorv1alpha1.PtahMigration:
		return true
	}
	return false
}

func stored(t *testing.T, c *cluster, family string) client.Object {
	t.Helper()
	object := resting(family)
	if err := c.Client.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatalf("read the resource: %v", err)
	}
	return object
}

func suspendOf(object client.Object) bool {
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		return typed.Spec.Suspend
	case *operatorv1alpha1.PtahMigration:
		return typed.Spec.Suspend
	}
	return false
}

// runCommand runs the plugin against api in namespace demo.
func runCommand(t *testing.T, api client.Client, arguments ...string) (int, string, string) {
	t.Helper()
	previous := connector
	connector = func(string, string, string, time.Duration) (client.Client, string, error) {
		return api, demo, nil
	}
	t.Cleanup(func() { connector = previous })
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), arguments, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func requireSays(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("%s is\n%s\nwhich does not say %q", what, text, want)
		}
	}
}

// Status steps the operator would write.

func schemaStatus(change func(*operatorv1alpha1.PtahSchemaStatus)) func(client.Object) {
	return func(object client.Object) { change(&object.(*operatorv1alpha1.PtahSchema).Status) }
}

func migrationStatus(change func(*operatorv1alpha1.PtahMigrationStatus)) func(client.Object) {
	return func(object client.Object) { change(&object.(*operatorv1alpha1.PtahMigration).Status) }
}

// unchanged is a poll that lands while the operator writes nothing.
func unchanged(client.Object) {}

// readAgain is the harvest of a read that began after the request: phase
// InSync, a reading at readAgainAt, nothing in flight.
func readAgain(object client.Object) {
	at := metav1.NewTime(readAgainAt)
	switch typed := object.(type) {
	case *operatorv1alpha1.PtahSchema:
		typed.Status.Phase, typed.Status.ActiveOperation = operatorv1alpha1.PhaseInSync, nil
		typed.Status.Target.LastObservedAt = &at
	case *operatorv1alpha1.PtahMigration:
		typed.Status.Phase, typed.Status.ActiveOperation = operatorv1alpha1.MigrationPhaseInSync, nil
		typed.Status.History.ObservedAt = at
	}
}

// The commands share the package-level connector, so these tests do not run
// in parallel.

func TestSuspendSetsTheFieldAndReportsWhatTheOperatorObserved(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			api := newCluster(t, resting(family))

			code, stdout, stderr := runCommand(t, api, "suspend", family+"/storefront")

			if code != exitOK || api.patchCount() != 1 || !suspendOf(stored(t, api, family)) {
				t.Fatalf("suspend = %d after %d patches; stderr %q", code, api.patchCount(), stderr)
			}
			requireSays(t, "suspend's report", stdout,
				"Suspend requested for ptah"+family+"/storefront in namespace demo.",
				"Requested: spec.suspend true, generation 4",
				"Observed:  generation 4, phase InSync",
				"The operator starts no new Job once it observes generation 4. Suspension cancels no SQL.",
				"kubectl ptah wait "+family+"/storefront --for suspended -n demo")
			if labels := stored(t, api, family).GetLabels(); labels["team"] != "shop" {
				t.Fatalf("suspend changed the labels to %v", labels)
			}

			code, stdout, _ = runCommand(t, api, "suspend", family+"/storefront")

			if code != exitOK || api.patchCount() != 1 {
				t.Fatalf("suspend again = %d after %d patches, want nothing sent", code, api.patchCount())
			}
			requireSays(t, "the second suspend's report", stdout, "spec.suspend of ptah"+family+"/storefront in namespace demo already holds true; nothing was sent.")

			code, stdout, _ = runCommand(t, api, "resume", "ptah"+family+"/storefront")

			if code != exitOK || api.patchCount() != 2 || suspendOf(stored(t, api, family)) {
				t.Fatalf("resume = %d after %d patches", code, api.patchCount())
			}
			requireSays(t, "resume's report", stdout, "Resume requested for", "Requested: spec.suspend false",
				"reads the database again and decides from what it finds",
				"kubectl ptah wait "+family+"/storefront --for in-sync -n demo")
		})
	}
}

// Suspension stops no SQL. What it does to the work in flight depends on the
// work, and for an Apply on the family; the report says which, one case at a
// time, by operate's reading of the controller (internal/controller holds
// that reading to the reconciler).
func TestSuspendSaysWhatBecomesOfTheWorkInFlight(t *testing.T) {
	schemaWith := func(phase operatorv1alpha1.ReconciliationPhase, operation string, jobUID string, proof bool) func() client.Object {
		return func() client.Object {
			schema := resting("schema").(*operatorv1alpha1.PtahSchema)
			schema.Status.Phase = phase
			if operation != "" {
				schema.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{
					Type: operatorv1alpha1.OperationType(operation), ID: "attempt", JobName: "job", JobUID: types.UID(jobUID),
				}
			}
			if proof {
				schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
					Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: "apply",
				}
			}
			return schema
		}
	}
	migrationWith := func(phase operatorv1alpha1.MigrationPhase, operation string, jobUID string) func() client.Object {
		return func() client.Object {
			migration := resting("migration").(*operatorv1alpha1.PtahMigration)
			migration.Status.Phase = phase
			migration.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{
				Type: operatorv1alpha1.MigrationOperationType(operation), ID: "attempt", JobName: "job", JobUID: types.UID(jobUID), Attempt: 1,
			}
			return migration
		}
	}
	for _, test := range []struct {
		name   string
		family string
		object func() client.Object
		says   []string
	}{
		{"an Apply running", "migration", migrationWith(operatorv1alpha1.MigrationPhaseApplying, "Apply", "job-uid"), []string{
			"Running:   Apply operation", "The Apply runs to its end: suspension cancels no SQL.",
			"records the run as one nobody accounted for (Unknown)", "kubectl ptah migration storefront -n demo shows the record"}},
		{"an Apply running", "schema", schemaWith(operatorv1alpha1.PhaseApplying, "Apply", "job-uid", false), []string{
			"Running:   Apply operation", "The Apply runs to its end: suspension cancels no SQL.",
			"records its outcome as unknown and records no applied plan",
			"waits for resume and for the Apply's execution deadline", "keeps its database lock"}},
		{"an Apply claim with no Job", "schema", schemaWith(operatorv1alpha1.PhaseApplying, "Apply", "", false), []string{
			"The Apply claim has not created its Job: the operator drops it, and no SQL runs."}},
		{"a read", "migration", migrationWith(operatorv1alpha1.MigrationPhaseReading, "History", "job-uid"), []string{
			"Running:   History operation", "The operator drops the History claim: a read changes nothing in the database."}},
		{"the read-back after a run", "migration", migrationWith(operatorv1alpha1.MigrationPhaseVerifyingHistory, "History", "job-uid"), []string{
			"The operator drops the read of the history after the last run, and reads the history again after resume."}},
		{"a proof owed", "schema", schemaWith(operatorv1alpha1.PhaseVerifyingConvergence, "", "", true), []string{
			"The proof that the last Apply converged starts no Job until resume", "keeps its database lock"}},
		{"a proof's Observe running", "schema", schemaWith(operatorv1alpha1.PhaseVerifyingConvergence, "Observe", "job-uid", true), []string{
			"Running:   Observe operation",
			"The Observe of the proof that the last Apply converged runs to its end, and the Plan that completes the proof waits for resume."}},
		{"a proof's Plan running", "schema", schemaWith(operatorv1alpha1.PhaseVerifyingConvergence, "Plan", "job-uid", true), []string{
			"The Plan that completes the proof that the last Apply converged runs to its end."}},
	} {
		t.Run(test.family+"/"+test.name, func(t *testing.T) {
			api := newCluster(t, test.object())

			code, stdout, stderr := runCommand(t, api, "suspend", test.family+"/storefront")

			if code != exitOK {
				t.Fatalf("suspend = %d, stderr %q", code, stderr)
			}
			requireSays(t, "suspend's report", stdout, test.says...)
			if strings.Contains(stdout, "cancels nothing") || strings.Contains(stdout, "outranks") {
				t.Fatalf("suspend's report claims more than the controller does: %q", stdout)
			}
		})
	}
}

func TestChangesExitByWhatTheAPIServerSaid(t *testing.T) {
	forbidden := func(message string) error {
		return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: http.StatusForbidden,
			Reason: metav1.StatusReasonForbidden, Message: message}}
	}
	schemas := k8sschema.GroupResource{Group: operatorv1alpha1.GroupVersion.Group, Resource: "ptahschemas"}
	for _, test := range []struct {
		name    string
		refuse  error
		readErr error
		replace bool
		code    int
		says    []string
	}{
		{name: "RBAC refuses the patch", refuse: forbidden(`ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob" cannot patch resource "ptahschemas" in API group "operator.ptah.run" in the namespace "demo"`),
			code: exitRefused, says: []string{`User "bob" cannot patch`, "Nothing was changed.", "needs get and patch on ptahschemas in namespace demo"}},
		{name: "admission refuses the patch", refuse: forbidden(`admission webhook "v.example" denied the request: not now`),
			code: exitRefused, says: []string{"denied the request: not now", "Nothing was changed."}},
		{name: "the patch has no answer", refuse: context.DeadlineExceeded,
			code: exitUnknown, says: []string{"no answer said whether it was stored", "kubectl ptah schema storefront -n demo shows what is stored"}},
		{name: "the API server fails the patch", refuse: apierrors.NewInternalError(errors.New("etcd")),
			code: exitUnknown, says: []string{"no answer said whether it was stored"}},
		{name: "the resource is replaced before the patch", replace: true,
			code: exitNotDone, says: []string{"deleted and created again", "Nothing was changed"}},
		{name: "the resource is not there", readErr: apierrors.NewNotFound(schemas, "storefront"), code: exitAbsent, says: []string{"not found"}},
		// Nothing was sent: the read is refused, not the change.
		{name: "RBAC refuses the read", readErr: forbidden(`ptahschemas.operator.ptah.run "storefront" is forbidden: User "bob" cannot get resource "ptahschemas" in API group "operator.ptah.run" in the namespace "demo"`),
			code: exitFailed, says: []string{`cannot get resource "ptahschemas"`}},
	} {
		for _, verb := range []string{"suspend", "refresh"} {
			t.Run(verb+"/"+test.name, func(t *testing.T) {
				api := newCluster(t, resting("schema"))
				api.refuse, api.readErr, api.replaced = test.refuse, test.readErr, test.replace

				code, stdout, stderr := runCommand(t, api, verb, "schema/storefront")

				if code != test.code || stdout != "" {
					t.Fatalf("%s = %d, stdout %q, stderr %q; want %d", verb, code, stdout, stderr, test.code)
				}
				requireSays(t, verb+"'s stderr", stderr, test.says...)
				if test.readErr != nil && api.patchCount() != 0 {
					t.Fatalf("%s sent %d patches after its read failed", verb, api.patchCount())
				}
			})
		}
	}
}

// A refresh writes the request and says that is all it did: the read has
// not happened yet, and it is not an Apply or an approval.
func TestRefreshSaysTheRequestIsNotTheRead(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			api := newCluster(t, resting(family))

			code, stdout, stderr := runCommand(t, api, "refresh", family+"/storefront")

			request := stored(t, api, family).GetAnnotations()[operatorv1alpha1.ReadRequestAnnotation]
			if code != exitOK || request == "" {
				t.Fatalf("refresh = %d, annotation %q, stderr %q", code, request, stderr)
			}
			if _, err := time.Parse(time.RFC3339, request); err != nil {
				t.Fatalf("refresh wrote %q, which asks for no read: %v", request, err)
			}
			requireSays(t, "refresh's report", stdout,
				"Read requested for ptah"+family+"/storefront in namespace demo.",
				"Request:  "+operatorv1alpha1.ReadRequestAnnotation+"="+request,
				"Observed: phase InSync, last read at 2026-10-01T09:00:00Z",
				"The request is written, not answered.",
				"a read already under way does not answer it",
				"It starts no Apply and approves nothing.",
				"kubectl ptah wait "+family+"/storefront --for read --read-request "+request+" -n demo waits for that read")
			if strings.Contains(stdout, "The resource is suspended") {
				t.Fatalf("refresh of a resource nobody suspended says %q", stdout)
			}
		})
	}

	t.Run("suspended", func(t *testing.T) {
		suspended := resting("schema").(*operatorv1alpha1.PtahSchema)
		suspended.Spec.Suspend = true
		api := newCluster(t, suspended)

		code, stdout, _ := runCommand(t, api, "refresh", "schema/storefront")

		if code != exitOK {
			t.Fatalf("refresh = %d", code)
		}
		requireSays(t, "refresh's report", stdout, "The resource is suspended: the operator takes the request after it is resumed.")
	})

	// The controller checks the engine and the realm before it reaches a
	// read request; a policy refusal comes after it.
	for _, row := range []struct {
		phase  operatorv1alpha1.MigrationPhase
		reason string
		held   bool
	}{
		{operatorv1alpha1.MigrationPhaseBlocked, "RealmConflict", true},
		{operatorv1alpha1.MigrationPhaseBlocked, "RealmNotAuthorized", true},
		{operatorv1alpha1.MigrationPhaseBlocked, "UnsupportedEngine", true},
		{operatorv1alpha1.MigrationPhaseBlocked, "DestructiveChangesDisabled", false},
		// A Ready reason left from a realm block on a resource that has
		// moved on is not a block.
		{operatorv1alpha1.MigrationPhaseResolving, "RealmConflict", false},
	} {
		t.Run(string(row.phase)+"/"+row.reason, func(t *testing.T) {
			blocked := resting("migration").(*operatorv1alpha1.PtahMigration)
			blocked.Status.Phase = row.phase
			blocked.Status.Conditions = []metav1.Condition{{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
				Reason: row.reason, Message: "held", ObservedGeneration: 4}}
			api := newCluster(t, blocked)

			code, stdout, _ := runCommand(t, api, "refresh", "migration/storefront")

			says := strings.Contains(stdout, "The resource is Blocked ("+row.reason+"): the operator checks that before it reaches a read request")
			if code != exitOK || says != row.held || (!row.held && strings.Contains(stdout, "is Blocked")) {
				t.Fatalf("refresh of a migration in %s with Ready reason %s = %d, says it takes no request: %t, want %t\n%s",
					row.phase, row.reason, code, says, row.held, stdout)
			}
		})
	}
}

// refresh --wait waits from the answer to its own patch: the operator takes
// the request, a read begins after it, and the wait ends on the document
// that holds that read's harvest and nothing in flight. When all of that
// lands before the wait's first read, only that baseline shows the reading
// is new.
func TestRefreshWaitsForTheReadItAskedFor(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps func(*cluster) []func(client.Object)
	}{
		{"read by read", func(api *cluster) []func(client.Object) {
			return []func(client.Object){
				unchanged,
				// The operator takes the request and claims the read.
				schemaStatus(func(status *operatorv1alpha1.PtahSchemaStatus) {
					status.LastReadRequest = operatorv1alpha1.ReadRequestToken(requestOn(api))
					status.Phase = operatorv1alpha1.PhaseResolving
					status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: "Resolve", ID: "read", JobName: "job"}
				}),
				readAgain,
			}
		}},
		{"before the first read", func(api *cluster) []func(client.Object) {
			return []func(client.Object){func(object client.Object) {
				object.(*operatorv1alpha1.PtahSchema).Status.LastReadRequest = operatorv1alpha1.ReadRequestToken(requestOn(api))
				readAgain(object)
			}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { refreshAndWait(t, test.steps) })
		})
	}
}

func refreshAndWait(t *testing.T, steps func(*cluster) []func(client.Object)) {
	api := newCluster(t, resting("schema"))
	api.then(steps(api)...)

	code, stdout, stderr := runCommand(t, api, "refresh", "schema/storefront", "--wait")

	if code != exitOK {
		t.Fatalf("refresh --wait = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireSays(t, "refresh --wait's report", stdout, "Read requested for", "ptahschema/storefront in namespace demo reached read.",
		"Database:   last read at 2026-10-01T09:07:00Z")
	requireSays(t, "refresh --wait's stderr", stderr, "Waiting up to 5m0s for ptahschema/storefront in namespace demo to reach read.")
	if strings.Contains(stdout, "--read-request") {
		t.Fatalf("refresh --wait points at a wait it already ran: %q", stdout)
	}
}

// requestOn is the read request stored on the schema, as the operator reads
// it before it answers.
func requestOn(api *cluster) string {
	return stored(api.t, api, "schema").GetAnnotations()[operatorv1alpha1.ReadRequestAnnotation]
}

// An InSync the resource already showed when the wait began is not an
// answer, however many times it is read again; a reading taken after it is.
func TestWaitDoesNotFinishOnAnOldInSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, resting("schema"))
		api.armed = true

		code, stdout, stderr := runCommand(t, api, "wait", "schema/storefront", "--for", "in-sync", "--timeout", "1m")

		if code != exitTimedOut || stdout != "" {
			t.Fatalf("wait on an old InSync = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "the timeout", stderr,
			"kubectl-ptah: ptahschema/storefront in namespace demo did not reach in-sync within 1m0s.",
			"Phase:       InSync",
			"Waiting for: a read of the database that began after the wait did (the last one is from 2026-10-01T09:00:00Z)",
			"kubectl ptah schema storefront -n demo shows every condition with its message.")
		if strings.Contains(stderr, "refresh") {
			t.Fatalf("a timeout with no read in flight points at refresh: %q", stderr)
		}
	})

	// A read in flight when the wait gave up is one a wait started again
	// skips, so the timeout points at a read request.
	synctest.Test(t, func(t *testing.T) {
		observing := resting("schema").(*operatorv1alpha1.PtahSchema)
		observing.Status.Phase = operatorv1alpha1.PhaseObserving
		observing.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: "Observe", ID: "read", JobName: "job", JobUID: "job-uid"}
		api := newCluster(t, observing)

		code, _, stderr := runCommand(t, api, "wait", "schema/storefront", "--for", "in-sync", "--timeout", "1m")

		if code != exitTimedOut {
			t.Fatalf("wait = %d, stderr %q", code, stderr)
		}
		requireSays(t, "the timeout", stderr, "A read of the database was under way. A wait started again does not count it",
			"kubectl ptah refresh schema/storefront --wait -n demo asks for a read that begins later")
	})

	// A wait for an Apply does not count readings, so a read in flight is
	// nothing a read request would help with.
	synctest.Test(t, func(t *testing.T) {
		observing := resting("schema").(*operatorv1alpha1.PtahSchema)
		observing.Status.Phase = operatorv1alpha1.PhaseObserving
		observing.Status.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: "Observe", ID: "read", JobName: "job", JobUID: "job-uid"}
		api := newCluster(t, observing)

		code, _, stderr := runCommand(t, api, "wait", "schema/storefront", "--for", "applied", "--timeout", "1m")

		if code != exitTimedOut || strings.Contains(stderr, "refresh") {
			t.Fatalf("wait for applied = %d, stderr %q; want a timeout that does not point at refresh", code, stderr)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, resting("schema"))
		api.armed = true
		api.then(unchanged, unchanged, readAgain)

		code, stdout, stderr := runCommand(t, api, "wait", "schema/storefront", "--for", "in-sync", "--timeout", "1m")

		if code != exitOK {
			t.Fatalf("wait with a fresh reading = %d, stderr %q", code, stderr)
		}
		requireSays(t, "the wait's report", stdout, "ptahschema/storefront in namespace demo reached in-sync.",
			"Phase:      InSync", "Database:   last read at 2026-10-01T09:07:00Z",
			"kubectl ptah schema storefront -n demo shows where it stands.")
	})
}

func TestWaitTimeoutNamesWhatHoldsTheResource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waiting := resting("schema").(*operatorv1alpha1.PtahSchema)
		waiting.Status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		waiting.Status.Conditions = []metav1.Condition{{Type: operatorv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: "AwaitingApproval", Message: "Plan is waiting for approval", ObservedGeneration: 4}}
		api := newCluster(t, waiting)

		code, _, stderr := runCommand(t, api, "wait", "schema/storefront", "--for", "in-sync", "--timeout", "30s")

		if code != exitTimedOut {
			t.Fatalf("wait = %d, stderr %q", code, stderr)
		}
		requireSays(t, "the timeout", stderr, "did not reach in-sync within 30s.", "Phase:       AwaitingApproval",
			"Held by:     Ready=False, AwaitingApproval: Plan is waiting for approval")
	})
}

func TestWaitStopsOnAnOutcomeTheTargetCannotFollow(t *testing.T) {
	api := newCluster(t, resting("migration"))

	code, stdout, stderr := runCommand(t, api, "wait", "migration/storefront", "--for", "suspended")

	if code != exitWillNotReach || stdout != "" {
		t.Fatalf("wait for suspension nobody asked for = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	requireSays(t, "the stop", stderr, "ptahmigration/storefront in namespace demo will not reach suspended: NotSuspended.",
		"Why:         spec.suspend is false", "kubectl ptah migration storefront -n demo shows where it stands.")
}

// With -o json stdout carries the resource the wait ended on and nothing
// else, as kubectl prints one.
func TestWaitPrintsTheResourceItEndedOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, resting("migration"))
		api.armed = true
		api.then(unchanged, readAgain)

		code, stdout, stderr := runCommand(t, api, "wait", "migration/storefront", "--for", "in-sync", "-o", "json")

		var migration operatorv1alpha1.PtahMigration
		if code != exitOK || yaml.UnmarshalStrict([]byte(stdout), &migration) != nil {
			t.Fatalf("wait -o json = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		if migration.Kind != "PtahMigration" || migration.APIVersion != "operator.ptah.run/v1alpha1" ||
			!migration.Status.History.ObservedAt.Time.Equal(readAgainAt) || stderr != "" {
			t.Fatalf("wait -o json printed %s %s read at %v, stderr %q", migration.APIVersion, migration.Kind, migration.Status.History, stderr)
		}
	})
}

// suspend --wait ends once the operator reports the generation the change
// asked for Suspended with nothing in flight, and not before.
func TestSuspendWaitsForTheOperator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, resting("migration"))
		suspended := migrationStatus(func(status *operatorv1alpha1.PtahMigrationStatus) {
			status.Phase = operatorv1alpha1.MigrationPhaseSuspended
		})
		api.then(unchanged, suspended)

		code, stdout, stderr := runCommand(t, api, "suspend", "migration/storefront", "--wait", "--wait-timeout", "1m")

		if code != exitOK {
			t.Fatalf("suspend --wait = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "suspend --wait's report", stdout, "Suspend requested for", "reached suspended.", "Phase:      Suspended")
		if strings.Contains(stdout, "kubectl ptah wait") {
			t.Fatalf("suspend --wait points at a wait it already ran: %q", stdout)
		}
	})
}

// A schema suspended while it owes the proof of an Apply is suspended, and
// the report says what the proof still holds. The controller leaves
// status.observedGeneration at the Apply's generation until the proof ends,
// and stamps the Suspended condition with the generation it acted on.
func TestWaitForSuspensionNamesAProofStillOwed(t *testing.T) {
	schema := resting("schema").(*operatorv1alpha1.PtahSchema)
	schema.Spec.Suspend = true
	schema.Status.Phase = operatorv1alpha1.PhaseSuspended
	schema.Status.ObservedGeneration = 3
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome: operatorv1alpha1.PendingObservationApplySucceeded, ApplyOperationID: "apply",
	}
	schema.Status.Conditions = append(schema.Status.Conditions, metav1.Condition{Type: operatorv1alpha1.ConditionSuspended,
		Status: metav1.ConditionTrue, Reason: "Requested", Message: "New database operations are suspended", ObservedGeneration: 4})

	// On the bubble's clock, as its neighbors are: a regression that makes
	// the target unreachable costs the five-minute default in fake time.
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, schema.DeepCopy())

		code, stdout, stderr := runCommand(t, api, "wait", "schema/storefront", "--for", "suspended")

		if code != exitOK {
			t.Fatalf("wait = %d, stderr %q", code, stderr)
		}
		requireSays(t, "the wait's report", stdout, "reached suspended.",
			"The proof that the last Apply converged waits for resume. Until then the schema keeps its database lock")
	})
}

// A schema resumed while it owes the proof of an Apply runs that proof
// before it reads the database again, and the report says so.
func TestResumeSaysAProofStillOwedRunsFirst(t *testing.T) {
	schema := resting("schema").(*operatorv1alpha1.PtahSchema)
	schema.Spec.Suspend = true
	schema.Status.Phase = operatorv1alpha1.PhaseSuspended
	schema.Status.PendingObservation = &operatorv1alpha1.PendingObservationStatus{
		Outcome: operatorv1alpha1.PendingObservationOutcomeUnknown, ApplyOperationID: "apply",
	}
	api := newCluster(t, schema)

	code, stdout, stderr := runCommand(t, api, "resume", "schema/storefront")

	if code != exitOK {
		t.Fatalf("resume = %d, stderr %q", code, stderr)
	}
	requireSays(t, "resume's report", stdout,
		"Once the operator observes generation 4 it runs the proof still owed after the last Apply, then reads the database again")
}

// A resource with no finalizer deleted while a wait reads it is gone before
// any document says so; the wait ends as it does for one being deleted.
func TestWaitStopsOnAResourceDeletedUnderIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newCluster(t, resting("migration"))
		api.armed = true
		// After the second read the API server answers that the migration
		// is gone, as it does for one deleted with no finalizer.
		api.then(unchanged, func(client.Object) {
			api.mu.Lock()
			defer api.mu.Unlock()
			api.readErr = apierrors.NewNotFound(k8sschema.GroupResource{Group: operatorv1alpha1.GroupVersion.Group, Resource: "ptahmigrations"}, "storefront")
		})

		code, stdout, stderr := runCommand(t, api, "wait", "migration/storefront", "--for", "in-sync")

		if code != exitWillNotReach || stdout != "" {
			t.Fatalf("wait on a deleted migration = %d, stdout %q, stderr %q", code, stdout, stderr)
		}
		requireSays(t, "the stop", stderr, "will not reach in-sync: Deleted.", "the resource was deleted")
	})
}

// The command lines the operate verbs refuse before they reach a cluster.
func TestOperateUsage(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{
		{"suspend"},
		{"suspend", "storefront"},
		{"suspend", "plan/storefront"},
		{"suspend", "schema/storefront", "migration/orders"},
		{"suspend", "schema/storefront", "--for", "in-sync"},
		{"suspend", "schema/storefront", "-o", "wide"},
		{"suspend", "schema/storefront", "--wait", "--wait-timeout", "0s"},
		{"resume", "schema/storefront", "--for", "suspended"},
		{"resume", "schema/storefront", "--for", "read"},
		{"resume", "schema/storefront", "--for", "soon"},
		{"refresh", "schema/storefront", "--for", "read"},
		{"wait", "schema/storefront"},
		{"wait", "schema/storefront", "--for", "ready"},
		{"wait", "schema/storefront", "--for", "read"},
		{"wait", "schema/storefront", "--for", "read", "--read-request", "yesterday"},
		{"wait", "schema/storefront", "--for", "in-sync", "--read-request", "2026-11-01T10:00:00Z"},
		{"wait", "migration/orders", "--for", "applied", "--plan", "ptah-mplan-1"},
		{"wait", "schema/storefront", "--for", "approval", "--plan", "ptah-plan-1"},
		{"wait", "schema/storefront", "--for", "in-sync", "--timeout", "0s"},
		{"approve", "schema/storefront", "--wait", "--dry-run", "client"},
		{"approve", "schema/storefront", "--wait", "--dry-run", "server"},
	} {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			code := run(context.Background(), arguments, &stdout, &stderr)

			if code != exitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("run(%q) = %d, stdout %q, stderr %q; want usage", arguments, code, stdout.String(), stderr.String())
			}
		})
	}
}

// approve --wait waits from the resource as it was before the approval
// existed, for the plan it approved and no other. Here the operator applies
// the plan before the wait's first read: only a baseline read before the
// create tells that Apply apart from one recorded earlier.
func TestApproveWaitsForTheApplyOfThePlanItApproved(t *testing.T) {
	applied := func(plan string) func(*operatorv1alpha1.PtahSchemaStatus) {
		return func(status *operatorv1alpha1.PtahSchemaStatus) {
			at := metav1.NewTime(readAgainAt)
			status.Phase, status.ActiveOperation, status.Target.LastObservedAt = operatorv1alpha1.PhaseInSync, nil, &at
			status.Applied = &operatorv1alpha1.AppliedStatus{
				PlanRef: operatorv1alpha1.ImmutableObjectReference{Name: plan, UID: types.UID("uid-" + plan)}, PlanFingerprint: "sha256:" + plan,
				CompletedAt: at,
			}
		}
	}
	for _, test := range []struct {
		name string
		plan func(current string) string
		code int
		says string
	}{
		{"the approved plan", func(current string) string { return current }, exitOK, "reached applied."},
		{"another plan", func(string) string { return "ptah-plan-another" }, exitWillNotReach, "OtherPlanApplied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api, fingerprint := approvable(t, nil)
				current := planOf(t, api)
				waiting := &afterApproval{admitting: api, steps: []func(*operatorv1alpha1.PtahSchemaStatus){
					applied(test.plan(current)),
				}}

				code, stdout, stderr := runApproveWith(t, waiting, planreadtest.Namespace, strings.NewReader(""), false,
					"schema/storefront", "--plan-fingerprint", fingerprint, "--wait")

				if code != test.code || len(api.created) != 1 {
					t.Fatalf("approve --wait = %d after %d creates; stdout %q, stderr %q", code, len(api.created), stdout, stderr)
				}
				requireSays(t, "approve --wait's output", stdout+stderr, "Approved:", test.says)
			})
		})
	}
}

// afterApproval is the API server with the operator applying a plan once an
// approval exists: each read of the schema after the create first applies
// the next step to its status.
type afterApproval struct {
	*admitting
	steps []func(*operatorv1alpha1.PtahSchemaStatus)
}

func (a *afterApproval) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, schema := object.(*operatorv1alpha1.PtahSchema); schema && len(a.created) > 0 && len(a.steps) > 0 {
		step := a.steps[0]
		a.steps = a.steps[1:]
		stored := &operatorv1alpha1.PtahSchema{}
		if err := a.admitting.Get(ctx, key, stored); err != nil {
			return err
		}
		step(&stored.Status)
		if err := a.admitting.Status().Update(ctx, stored); err != nil {
			return err
		}
	}
	return a.admitting.Get(ctx, key, object, options...)
}

func planOf(t *testing.T, api client.Client) string {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	return schema.Status.Plan.Name
}
