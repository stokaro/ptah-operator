package approve_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/approve"
	"github.com/stokaro/ptah-operator/pkg/planread/planreadtest"
)

func TestParseTargetNamesTheFamily(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		target string
		family approve.Family
		name   string
	}{
		{"schema/storefront", approve.Schema, "storefront"},
		{"migration/shipments", approve.Migration, "shipments"},
	} {
		family, name, err := approve.ParseTarget(test.target)
		if err != nil || family != test.family || name != test.name {
			t.Fatalf("ParseTarget(%q) = %q, %q, %v; want %q, %q", test.target, family, name, err, test.family, test.name)
		}
	}
	// A bare name could be a schema or a migration of the same name, and an
	// approval for the wrong one is an approval nobody reviewed.
	for _, target := range []string{"storefront", "schema/", "/storefront", "schemas/storefront",
		"ptahschema/storefront", "plan/storefront", "schema/a/b"} {
		if _, _, err := approve.ParseTarget(target); err == nil {
			t.Fatalf("ParseTarget(%q) accepted it", target)
		}
	}
}

// waiting stores a schema whose current plan waits for a person, the state in
// which admission takes an approval.
func waiting(t *testing.T) (*planreadtest.Lab, *operatorv1alpha1.PtahSchemaPlan) {
	t.Helper()
	lab := planreadtest.NewLab(t, "storefront")
	plan, _ := lab.Publish(t, []string{"ALTER TABLE orders DROP COLUMN legacy_note;"}, "drop")
	lab.SetCurrent(t, plan)
	setStatus(t, lab, func(status *operatorv1alpha1.PtahSchemaStatus) {
		status.Phase = operatorv1alpha1.PhaseAwaitingApproval
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionTrue,
			Reason: string(operatorv1alpha1.ReasonPlanReady), Message: "An exact immutable plan is ready for approval",
		})
	})
	return lab, plan
}

func setStatus(t *testing.T, lab *planreadtest.Lab, change func(*operatorv1alpha1.PtahSchemaStatus)) {
	t.Helper()
	schema := &operatorv1alpha1.PtahSchema{}
	key := client.ObjectKey{Namespace: planreadtest.Namespace, Name: "storefront"}
	if err := lab.Client().Get(context.Background(), key, schema); err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	change(&schema.Status)
	if err := lab.Client().Status().Update(context.Background(), schema); err != nil {
		t.Fatalf("update the schema status: %v", err)
	}
}

func TestLoadReadsTheVerifiedPlanAndTheIdentitiesAnApprovalCarries(t *testing.T) {
	t.Parallel()
	lab, plan := waiting(t)

	review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !review.Open || review.Closed != "" {
		t.Fatalf("Load() says closed: %q", review.Closed)
	}
	if review.ResourceUID != lab.Schema().UID || review.PlanName != plan.Name || review.PlanUID != plan.UID ||
		review.Fingerprint != plan.Spec.Fingerprint || review.ContractVersion != plan.Spec.ContractVersion {
		t.Fatalf("Load() identities = %+v, want the schema's and plan %s's", review, plan.Name)
	}
	if review.SchemaPlan == nil || len(review.SchemaPlan.Statements) != 1 ||
		review.SchemaPlan.Statements[0].SQL != "ALTER TABLE orders DROP COLUMN legacy_note;" {
		t.Fatalf("Load() did not carry the verified statements: %+v", review.SchemaPlan)
	}
}

func TestLoadSaysWhyAnApprovalWouldNotBeAdmitted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahSchemaStatus)
		says   string
	}{
		{"approved already", func(s *operatorv1alpha1.PtahSchemaStatus) {
			s.Plan.Approval = &operatorv1alpha1.ConsumedApprovalStatus{Name: "first",
				Approver: operatorv1alpha1.ApprovalIdentity{Username: "alice"}}
		}, "approved already, by alice"},
		// The operator reads a waiting schema again at its interval; the plan
		// stays current and admission refuses an approval meanwhile.
		{"reading again", func(s *operatorv1alpha1.PtahSchemaStatus) {
			s.Phase = operatorv1alpha1.PhaseObserving
			s.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationObserve, ID: "op"}
		}, "running Observe"},
		{"between two operations of a read", func(s *operatorv1alpha1.PtahSchemaStatus) {
			s.Phase = operatorv1alpha1.PhasePlanning
		}, "is Planning, not waiting"},
		{"ready to apply without one", func(s *operatorv1alpha1.PtahSchemaStatus) {
			s.Phase = operatorv1alpha1.PhaseReadyToApply
		}, "is ReadyToApply"},
		{"no approval required", func(s *operatorv1alpha1.PtahSchemaStatus) {
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type: operatorv1alpha1.ConditionApprovalRequired, Status: metav1.ConditionFalse, Reason: "NotRequired"})
		}, "does not require an approval"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab, _ := waiting(t)
			setStatus(t, lab, test.change)

			review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if review.Open || !strings.Contains(review.Closed, test.says) {
				t.Fatalf("Load() open = %t, closed = %q; want closed naming %q", review.Open, review.Closed, test.says)
			}
		})
	}
}

func TestLoadAnswersAbsenceAsAnAnswer(t *testing.T) {
	t.Parallel()
	lab := planreadtest.NewLab(t, "storefront")
	if _, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront"); !errors.Is(err, approve.ErrNothingToApprove) {
		t.Fatalf("Load() of a schema with no plan = %v, want ErrNothingToApprove", err)
	}
	if _, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "absent"); !errors.Is(err, approve.ErrNotFound) {
		t.Fatalf("Load() of no schema = %v, want ErrNotFound", err)
	}
	if _, err := approve.Load(context.Background(), lab.Client(), approve.Migration, planreadtest.Namespace, "storefront"); !errors.Is(err, approve.ErrNotFound) {
		t.Fatalf("Load() of a migration named like a schema = %v, want ErrNotFound", err)
	}
}

// answering is the lab's API server with the answer a test chooses for the
// approval's create.
type answering struct {
	client.Client
	answer  func(client.Object) error
	created []client.Object
	options []client.CreateOption
}

func (a *answering) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	a.created = append(a.created, object)
	a.options = append(a.options, options...)
	if a.answer != nil {
		if err := a.answer(object); err != nil {
			return err
		}
	}
	return a.Client.Create(ctx, object, options...)
}

func statusError(code int32, reason metav1.StatusReason, message string) error {
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: code, Reason: reason, Message: message,
	}}
}

func TestSubmitCreatesTheApprovalOfTheReviewedPlan(t *testing.T) {
	t.Parallel()
	lab, plan := waiting(t)
	review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	api := &answering{Client: lab.Client()}

	result := approve.Submit(context.Background(), api, review, approve.Options{})

	if result.Outcome != approve.Admitted || len(api.created) != 1 {
		t.Fatalf("Submit() = %+v after %d creates, want admitted after one", result, len(api.created))
	}
	approval, ok := api.created[0].(*operatorv1alpha1.PtahSchemaApproval)
	if !ok {
		t.Fatalf("Submit() created a %T", api.created[0])
	}
	spec := approval.Spec
	if spec.SchemaRef.Name != "storefront" || spec.SchemaRef.UID != lab.Schema().UID ||
		spec.PlanRef.Name != plan.Name || spec.PlanRef.UID != plan.UID || spec.PlanFingerprint != plan.Spec.Fingerprint {
		t.Fatalf("the approval binds %+v, want schema storefront and plan %s", spec, plan.Name)
	}
	// Admission stamps who approved and when; a value written here is refused.
	if spec.Approver.Username != "" || !spec.ApprovedAt.IsZero() || spec.MutationRequestUID != "" {
		t.Fatalf("the approval carries an identity admission did not stamp: %+v", spec.Approver)
	}
	if approval.GenerateName != "storefront-" || approval.Name == "" {
		t.Fatalf("the approval is named %q from %q, want a name generated from storefront-", approval.Name, approval.GenerateName)
	}
}

func TestSubmitSendsNothingForAnotherPlan(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*testing.T, *planreadtest.Lab, *operatorv1alpha1.PtahSchemaPlan)
		says   string
	}{
		{"a newer plan replaced it", func(t *testing.T, lab *planreadtest.Lab, _ *operatorv1alpha1.PtahSchemaPlan) {
			newer, _ := lab.Publish(t, []string{"ALTER TABLE orders DROP COLUMN legacy_note;", "DROP TABLE audit;"}, "newer")
			lab.SetCurrent(t, newer)
		}, "replaced"},
		{"the plan was recreated under its name", func(t *testing.T, lab *planreadtest.Lab, plan *operatorv1alpha1.PtahSchemaPlan) {
			lab.Recreate(t, plan)
			lab.SetCurrent(t, plan)
			setStatus(t, lab, func(s *operatorv1alpha1.PtahSchemaStatus) { s.Plan.UID = types.UID("uid-recreated-" + plan.Name) })
		}, "replaced"},
		{"the schema was deleted and created again", func(t *testing.T, lab *planreadtest.Lab, _ *operatorv1alpha1.PtahSchemaPlan) {
			schema := lab.Schema()
			if err := lab.Client().Delete(context.Background(), schema); err != nil {
				t.Fatalf("delete the schema: %v", err)
			}
			schema.ResourceVersion, schema.UID = "", "uid-another-storefront"
			if err := lab.Client().Create(context.Background(), schema); err != nil {
				t.Fatalf("create the schema again: %v", err)
			}
		}, "deleted"},
		// The status names the same plan object with another fingerprint:
		// whatever wrote it, it is not the binding that was reviewed.
		{"the fingerprint the status names changed", func(t *testing.T, lab *planreadtest.Lab, _ *operatorv1alpha1.PtahSchemaPlan) {
			setStatus(t, lab, func(s *operatorv1alpha1.PtahSchemaStatus) { s.Plan.Fingerprint = "sha256:another" })
		}, "carries fingerprint sha256:another"},
		{"the schema was deleted", func(t *testing.T, lab *planreadtest.Lab, _ *operatorv1alpha1.PtahSchemaPlan) {
			if err := lab.Client().Delete(context.Background(), lab.Schema()); err != nil {
				t.Fatalf("delete the schema: %v", err)
			}
		}, "deleted after you reviewed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab, plan := waiting(t)
			review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			test.change(t, lab, plan)
			api := &answering{Client: lab.Client()}

			result := approve.Submit(context.Background(), api, review, approve.Options{})

			if result.Outcome != approve.Changed || !strings.Contains(result.Message, test.says) ||
				!strings.Contains(result.Message, "Nothing was approved") {
				t.Fatalf("Submit() = %+v, want changed naming %q", result, test.says)
			}
			if len(api.created) != 0 {
				t.Fatalf("Submit() sent %d approvals for a plan nobody reviewed", len(api.created))
			}
		})
	}
}

func TestSubmitSendsNothingWhileTheOperatorReadsAgain(t *testing.T) {
	t.Parallel()
	lab, _ := waiting(t)
	review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	setStatus(t, lab, func(s *operatorv1alpha1.PtahSchemaStatus) {
		s.Phase = operatorv1alpha1.PhaseResolving
		s.ActiveOperation = &operatorv1alpha1.ActiveOperationStatus{Type: operatorv1alpha1.OperationResolve, ID: "op"}
	})
	api := &answering{Client: lab.Client()}

	result := approve.Submit(context.Background(), api, review, approve.Options{})

	if result.Outcome != approve.Closed || !strings.Contains(result.Message, "running Resolve") || len(api.created) != 0 {
		t.Fatalf("Submit() = %+v after %d creates, want closed and nothing sent", result, len(api.created))
	}
}

func TestSubmitPassesTheAPIServersAnswerOn(t *testing.T) {
	t.Parallel()
	const rbac = `ptahschemaapprovals.operator.ptah.run is forbidden: User "bob" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "application"`
	const admission = `admission webhook "vptahschemaapproval.operator.ptah.run" denied the request: an approver may not approve a plan for a change they authored`
	for _, test := range []struct {
		name    string
		answer  error
		outcome approve.Outcome
		says    string
	}{
		{"RBAC refuses the create", statusError(http.StatusForbidden, metav1.StatusReasonForbidden, rbac), approve.Forbidden, rbac},
		// Admission refuses with 403 as well, in its own words: it is not RBAC.
		{"admission refuses it", statusError(http.StatusForbidden, metav1.StatusReasonForbidden, admission), approve.Refused, admission},
		{"the schema refuses a field", statusError(http.StatusUnprocessableEntity, metav1.StatusReasonInvalid,
			`PtahSchemaApproval.operator.ptah.run "x" is invalid: spec.planFingerprint: Invalid value`), approve.Refused, "spec.planFingerprint"},
		{"no answer in time", context.DeadlineExceeded, approve.Unknown, "may have been stored"},
		{"the server failed", statusError(http.StatusInternalServerError, metav1.StatusReasonInternalError, "etcd"), approve.Unknown, "may have been stored"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab, _ := waiting(t)
			review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			api := &answering{Client: lab.Client(), answer: func(client.Object) error { return test.answer }}

			result := approve.Submit(context.Background(), api, review, approve.Options{})

			if result.Outcome != test.outcome || !strings.Contains(result.Message, test.says) || result.Approval != nil {
				t.Fatalf("Submit() = %+v, want %s naming %q", result, test.outcome, test.says)
			}
		})
	}
}

func TestSubmitAsksAdmissionWithoutStoringOnADryRun(t *testing.T) {
	t.Parallel()
	lab, _ := waiting(t)
	review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	api := &answering{Client: lab.Client(), answer: func(client.Object) error { return nil }}

	result := approve.Submit(context.Background(), api, review, approve.Options{ServerDryRun: true})

	if result.Outcome != approve.Previewed || len(api.created) != 1 {
		t.Fatalf("Submit() = %+v after %d creates, want previewed after one", result, len(api.created))
	}
	dryRun := false
	for _, option := range api.options {
		if option == client.DryRunAll {
			dryRun = true
		}
	}
	if !dryRun {
		t.Fatal("the create was not sent as a dry run")
	}
}

func TestManifestCarriesTheBindingAndNoIdentity(t *testing.T) {
	t.Parallel()
	review := approve.Review{
		Family: approve.Migration, Namespace: "application", Name: "shipments", ResourceUID: "uid-m",
		PlanName: "shipments-p-3", PlanUID: "uid-p", Fingerprint: "sha256:aa",
	}

	approval, ok := approve.Manifest(review, "shipments-approval").(*operatorv1alpha1.PtahMigrationApproval)
	if !ok {
		t.Fatal("Manifest() of a migration is not a PtahMigrationApproval")
	}
	if approval.Name != "shipments-approval" || approval.GenerateName != "" || approval.Namespace != "application" {
		t.Fatalf("Manifest() named %q/%q in %q", approval.Name, approval.GenerateName, approval.Namespace)
	}
	want := operatorv1alpha1.PtahMigrationApprovalSpec{
		MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: "shipments", UID: "uid-m"},
		PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: "shipments-p-3", UID: "uid-p"},
		PlanFingerprint: "sha256:aa",
	}
	if !reflect.DeepEqual(approval.Spec, want) {
		t.Fatalf("Manifest() spec = %+v, want %+v", approval.Spec, want)
	}
	if kind := approval.GetObjectKind().GroupVersionKind(); kind.Kind != "PtahMigrationApproval" || kind.Group != "operator.ptah.run" {
		t.Fatalf("Manifest() kind = %v", kind)
	}
}

// waitingMigration stores a migration whose plan waits for a person, with the
// history read the plan was computed against.
func waitingMigration(change func(*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan)) []client.Object {
	migration := &operatorv1alpha1.PtahMigration{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments", UID: "uid-shipments"},
		Status: operatorv1alpha1.PtahMigrationStatus{
			Phase:   operatorv1alpha1.MigrationPhaseAwaitingApproval,
			History: &operatorv1alpha1.MigrationHistoryStatus{CurrentVersion: 13},
			Plan:    &operatorv1alpha1.ImmutableObjectReference{Name: "shipments-p-14", UID: "uid-p-14"},
			Conditions: []metav1.Condition{{
				Type: operatorv1alpha1.ConditionMigrationApprovalRequired, Status: metav1.ConditionTrue, Reason: "Waiting",
			}},
		},
	}
	plan := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments-p-14", UID: "uid-p-14"},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{
			ContractVersion: migrationContract,
			MigrationRef:    operatorv1alpha1.ImmutableObjectReference{Name: "shipments", UID: "uid-shipments"},
			Fingerprint:     "sha256:14",
		},
	}
	if change != nil {
		change(migration, plan)
	}
	return []client.Object{migration, plan}
}

// migrationContract is a plan contract the operator reads.
const migrationContract = 1

func TestLoadReadsAMigrationsPlanWithoutClaimingSQL(t *testing.T) {
	t.Parallel()
	api := planreadtest.NewClient(t, waitingMigration(nil)...)

	review, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !review.Open || review.ResourceUID != "uid-shipments" || review.PlanName != "shipments-p-14" ||
		review.PlanUID != "uid-p-14" || review.Fingerprint != "sha256:14" || review.MigrationPlan == nil {
		t.Fatalf("Load() = %+v, want the open plan shipments-p-14", review)
	}
	if review.SchemaPlan != nil {
		t.Fatal("Load() of a migration carries schema statements it never read")
	}
}

func TestLoadSaysWhyAMigrationApprovalWouldNotBeAdmitted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahMigration, *operatorv1alpha1.PtahMigrationPlan)
		says   string
	}{
		{"history not read", func(m *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
			m.Status.History = nil
		}, "not read the database's history"},
		{"reading the history again", func(m *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
			m.Status.Phase = operatorv1alpha1.MigrationPhaseReading
			m.Status.ActiveOperation = &operatorv1alpha1.MigrationOperationStatus{Type: operatorv1alpha1.MigrationOperationHistory, ID: "op"}
		}, "running History"},
		{"another phase", func(m *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
			m.Status.Phase = operatorv1alpha1.MigrationPhasePlanning
		}, "is Planning"},
		// A plan of the same name stored for another migration is not this
		// migration's plan, whatever its fingerprint says.
		{"the plan belongs to another migration", func(_ *operatorv1alpha1.PtahMigration, p *operatorv1alpha1.PtahMigrationPlan) {
			p.Spec.MigrationRef.UID = "uid-another-shipments"
		}, "is not the plan"},
		{"another object under the plan's name", func(_ *operatorv1alpha1.PtahMigration, p *operatorv1alpha1.PtahMigrationPlan) {
			p.UID = "uid-p-14-again"
		}, "is not the plan"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api := planreadtest.NewClient(t, waitingMigration(test.change)...)

			review, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if review.Open || !strings.Contains(review.Closed, test.says) {
				t.Fatalf("Load() open = %t, closed = %q; want closed naming %q", review.Open, review.Closed, test.says)
			}
		})
	}
}

func TestLoadRefusesAMigrationPlanInAContractItCannotRead(t *testing.T) {
	t.Parallel()
	api := planreadtest.NewClient(t, waitingMigration(func(_ *operatorv1alpha1.PtahMigration, p *operatorv1alpha1.PtahMigrationPlan) {
		p.Spec.ContractVersion = 99
	})...)

	if _, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments"); err == nil ||
		!strings.Contains(err.Error(), "contract 99") {
		t.Fatalf("Load() error = %v, want a refusal naming contract 99", err)
	}
}

func TestSubmitApprovesTheReviewedMigrationPlanOnly(t *testing.T) {
	t.Parallel()
	objects := waitingMigration(nil)
	api := &answering{Client: planreadtest.NewClient(t, objects...)}
	review, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	result := approve.Submit(context.Background(), api, review, approve.Options{Name: "shipments-14"})

	if result.Outcome != approve.Admitted || len(api.created) != 1 {
		t.Fatalf("Submit() = %+v after %d creates, want admitted after one", result, len(api.created))
	}
	approval := api.created[0].(*operatorv1alpha1.PtahMigrationApproval)
	if approval.Name != "shipments-14" || approval.Spec.MigrationRef.UID != "uid-shipments" ||
		approval.Spec.PlanRef.UID != "uid-p-14" || approval.Spec.PlanFingerprint != "sha256:14" {
		t.Fatalf("the approval binds %+v", approval.Spec)
	}

	// The operator read the history again and planned 15 as well: the plan
	// the person reviewed is gone, and nothing more is sent.
	newer := &operatorv1alpha1.PtahMigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "application", Name: "shipments-p-15", UID: "uid-p-15"},
		Spec: operatorv1alpha1.PtahMigrationPlanSpec{ContractVersion: migrationContract,
			MigrationRef: operatorv1alpha1.ImmutableObjectReference{Name: "shipments", UID: "uid-shipments"}, Fingerprint: "sha256:15"},
	}
	if err := api.Client.Create(context.Background(), newer); err != nil {
		t.Fatalf("store the newer plan: %v", err)
	}
	migration := &operatorv1alpha1.PtahMigration{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: "application", Name: "shipments"}, migration); err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	migration.Status.Plan = &operatorv1alpha1.ImmutableObjectReference{Name: newer.Name, UID: newer.UID}
	if err := api.Client.Update(context.Background(), migration); err != nil {
		t.Fatalf("point the migration at the newer plan: %v", err)
	}

	again := approve.Submit(context.Background(), api, review, approve.Options{Name: "shipments-14-retry"})

	if again.Outcome != approve.Changed || !strings.Contains(again.Message, "shipments-p-15") || len(api.created) != 1 {
		t.Fatalf("Submit() after the plan moved = %+v after %d creates, want changed and no second create", again, len(api.created))
	}
}

// racing is a reader that lets the operator replace the current plan right
// after the schema's status is read, before the plan is.
type racing struct {
	client.Reader
	replace func()
	done    bool
}

func (r *racing) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, object, options...)
	if _, schema := object.(*operatorv1alpha1.PtahSchema); schema && !r.done {
		r.done = true
		r.replace()
	}
	return err
}

func TestLoadShowsNoPlanUnderAnotherPlansIdentity(t *testing.T) {
	t.Parallel()
	lab, _ := waiting(t)
	reader := &racing{Reader: lab.Client(), replace: func() {
		newer, _ := lab.Publish(t, []string{"DROP TABLE audit;"}, "newer")
		lab.SetCurrent(t, newer)
	}}

	_, err := approve.Load(context.Background(), reader, approve.Schema, planreadtest.Namespace, "storefront")

	if err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("Load() error = %v, want the plan replaced between the two reads refused", err)
	}
}
