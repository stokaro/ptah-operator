package approve_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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
	// The typed client decodes the answer without its kind; what is printed
	// carries it.
	if kind := result.Approval.GetObjectKind().GroupVersionKind(); kind != operatorv1alpha1.GroupVersion.WithKind("PtahSchemaApproval") {
		t.Fatalf("the admitted approval reads as kind %v", kind)
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
	// The manager's own read refused inside admission comes back as a 403
	// whose text carries an authorizer's refusal, of the manager's identity.
	const managerRefused = `admission webhook "mapproval.operator.ptah.run" denied the request: read referenced plan: ptahschemaplans.operator.ptah.run "p" is forbidden: User "system:serviceaccount:ptah-system:ptah-operator" cannot get resource "ptahschemaplans" in API group "operator.ptah.run" in the namespace "application"`
	const vanished = `admission webhook "mapproval.operator.ptah.run" denied the request: read referenced plan: ptahschemaplans.operator.ptah.run "p" not found`
	for _, test := range []struct {
		name    string
		answer  error
		outcome approve.Outcome
		says    []string
	}{
		{"RBAC refuses the create", statusError(http.StatusForbidden, metav1.StatusReasonForbidden, rbac), approve.Forbidden, []string{rbac, "Nothing was approved"}},
		{"RBAC refuses a named create", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemaapprovals.operator.ptah.run "mine" is forbidden: User "bob" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "application"`),
			approve.Forbidden, []string{`"mine" is forbidden`}},
		// The API server writes the username with %q, so a quote in it is
		// escaped and the refusal is still RBAC's.
		{"RBAC refuses a user with a quote in the name", statusError(http.StatusForbidden, metav1.StatusReasonForbidden,
			`ptahschemaapprovals.operator.ptah.run is forbidden: User "bob \"b\" o'brien" cannot create resource "ptahschemaapprovals" in API group "operator.ptah.run" in the namespace "application"`),
			approve.Forbidden, []string{`o'brien`}},
		// Admission refuses with 403 as well, in its own words: it is not RBAC.
		{"admission refuses it", statusError(http.StatusForbidden, metav1.StatusReasonForbidden, admission), approve.Refused, []string{admission, "Nothing was approved"}},
		{"admission passes on the manager's own refusal", statusError(http.StatusForbidden, "", managerRefused), approve.Refused, []string{managerRefused}},
		{"the schema refuses a field", statusError(http.StatusUnprocessableEntity, metav1.StatusReasonInvalid,
			`PtahSchemaApproval.operator.ptah.run "x" is invalid: spec.planFingerprint: Invalid value`), approve.Refused, []string{"spec.planFingerprint"}},
		// The webhook answers 404 when the plan it reads is gone.
		{"the plan vanished under admission", statusError(http.StatusNotFound, "", vanished), approve.Refused,
			[]string{vanished, "something the approval names is gone", "kubectl ptah schema storefront -n " + planreadtest.Namespace}},
		{"the credentials are not accepted", statusError(http.StatusUnauthorized, metav1.StatusReasonUnauthorized, "Unauthorized"),
			approve.Refused, []string{"did not accept your credentials"}},
		{"the API server is too busy", statusError(http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "the server has received too many requests"),
			approve.Refused, []string{"too busy", "nothing was stored", "Run the command again"}},
		{"no answer in time", context.DeadlineExceeded, approve.Unknown,
			[]string{"did not answer in time", "may have been stored", "kubectl ptah schema storefront -n " + planreadtest.Namespace}},
		{"the connection dropped", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, approve.Unknown,
			[]string{"The answer was lost", "connection reset by peer", "may have been stored"}},
		{"the server failed", statusError(http.StatusInternalServerError, metav1.StatusReasonInternalError, "etcd"), approve.Unknown, []string{"may have been stored"}},
		{"the server timed out storing it", statusError(http.StatusGatewayTimeout, metav1.StatusReasonTimeout, "the request timed out"),
			approve.Unknown, []string{"did not answer in time", "may have been stored"}},
		{"admission timed out", statusError(http.StatusInternalServerError, metav1.StatusReasonInternalError,
			`Internal error occurred: failed calling webhook "mapproval.operator.ptah.run": context deadline exceeded`), approve.Unknown, []string{"may have been stored"}},
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

			if result.Outcome != test.outcome || result.Approval != nil {
				t.Fatalf("Submit() = %+v, want %s", result, test.outcome)
			}
			for _, says := range test.says {
				if !strings.Contains(result.Message, says) {
					t.Fatalf("Submit() said %q, want it to name %q", result.Message, says)
				}
			}
		})
	}
}

// A name already taken is a definite answer: nothing was stored. What holds
// the name decides what to tell the person, and an approval of the same plan
// under it is a decision already recorded, by whoever made it.
func TestSubmitSaysWhatHoldsATakenName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		stored func(*operatorv1alpha1.PtahSchemaApproval)
		says   string
	}{
		{"an approval of the same plan", func(*operatorv1alpha1.PtahSchemaApproval) {}, "an approval of this plan is recorded, and this request stored nothing"},
		{"an approval of another plan", func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.PlanRef.UID = "uid-an-earlier-plan" }, "binds another plan"},
		{"the same plan under another fingerprint", func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.PlanFingerprint = "sha256:another" }, "binds another plan"},
		{"the same plan of another schema", func(a *operatorv1alpha1.PtahSchemaApproval) { a.Spec.SchemaRef.UID = "uid-another-storefront" }, "binds another plan"},
		{"nothing this identity may read", nil, "The name is taken"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lab, plan := waiting(t)
			review, err := approve.Load(context.Background(), lab.Client(), approve.Schema, planreadtest.Namespace, "storefront")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			api := &answering{Client: lab.Client()}
			if test.stored != nil {
				existing := &operatorv1alpha1.PtahSchemaApproval{
					ObjectMeta: metav1.ObjectMeta{Namespace: planreadtest.Namespace, Name: "mine"},
					Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
						SchemaRef:       operatorv1alpha1.ImmutableObjectReference{Name: "storefront", UID: review.ResourceUID},
						PlanRef:         operatorv1alpha1.ImmutableObjectReference{Name: plan.Name, UID: plan.UID},
						PlanFingerprint: plan.Spec.Fingerprint,
					},
				}
				test.stored(existing)
				if err := lab.Client().Create(context.Background(), existing); err != nil {
					t.Fatalf("store the approval that holds the name: %v", err)
				}
			} else {
				api.answer = func(client.Object) error {
					return apierrors.NewAlreadyExists(operatorv1alpha1.GroupVersion.WithResource("ptahschemaapprovals").GroupResource(), "mine")
				}
			}

			result := approve.Submit(context.Background(), api, review, approve.Options{Name: "mine"})

			if result.Outcome != approve.Refused || !strings.Contains(result.Message, `"mine" already exists`) ||
				!strings.Contains(result.Message, test.says) || result.Approval != nil {
				t.Fatalf("Submit() = %+v, want refused naming %q", result, test.says)
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

// The manifest is what a repository holds and a later apply sends back, so
// it carries the decision and nothing else: no empty approver, no null time,
// no status, no generated name, all of which kubectl or admission refuses.
func TestManifestCarriesTheDecisionAndNothingElse(t *testing.T) {
	t.Parallel()
	const fingerprint = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		family approve.Family
		kind   string
		owner  string
	}{
		{approve.Migration, "PtahMigrationApproval", "migrationRef"},
		{approve.Schema, "PtahSchemaApproval", "schemaRef"},
	} {
		review := approve.Review{
			Family: test.family, Namespace: "application", Name: "shipments", ResourceUID: "uid-m",
			PlanName: "shipments-p-3", PlanUID: "uid-p", Fingerprint: fingerprint,
		}
		want := map[string]any{
			"apiVersion": "operator.ptah.run/v1alpha1",
			"kind":       test.kind,
			"metadata":   map[string]any{"name": "shipments-approval", "namespace": "application"},
			"spec": map[string]any{
				test.owner:        map[string]any{"name": "shipments", "uid": "uid-m"},
				"planRef":         map[string]any{"name": "shipments-p-3", "uid": "uid-p"},
				"planFingerprint": fingerprint,
			},
		}

		if got := approve.Manifest(review, "shipments-approval").Object; !reflect.DeepEqual(got, want) {
			t.Fatalf("Manifest() of a %s = %v, want exactly %v", test.family, got, want)
		}
		if name := approve.Manifest(review, "").GetName(); name != "shipments-0123456789ab" {
			t.Fatalf("Manifest() without a name is named %q, want shipments-0123456789ab", name)
		}
	}
}

func TestDefaultNameIsOneValidNamePerPlan(t *testing.T) {
	t.Parallel()
	const fingerprint = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	review := func(name, fingerprint string) approve.Review {
		return approve.Review{Family: approve.Schema, Name: name, Fingerprint: fingerprint}
	}
	// A cut at the bound lands on a dot, then on a dash: neither may end a
	// label, so neither may precede the suffix.
	long := strings.Repeat("a", 239) + ".b" + strings.Repeat("c", 20)
	dashed := strings.Repeat("a", 239) + "-b" + strings.Repeat("c", 20)
	for _, test := range []struct {
		name, fingerprint, want string
	}{
		{"storefront", fingerprint, "storefront-0123456789ab"},
		{"storefront", "SHA256:ABCDEF0123456789ABCDEF", "storefront-abcdef012345"},
		{long, fingerprint, strings.Repeat("a", 239) + "-0123456789ab"},
		{dashed, fingerprint, strings.Repeat("a", 239) + "-0123456789ab"},
	} {
		if got := approve.DefaultName(review(test.name, test.fingerprint)); got != test.want {
			t.Fatalf("DefaultName(%q, %q) = %q, want %q", test.name, test.fingerprint, got, test.want)
		}
	}
	// Whatever a fingerprint is written in, the name stays one name per
	// fingerprint and a valid one.
	seen := map[string]string{}
	for _, value := range []string{fingerprint, "sha256:aa", "v2:Not_Hex!", "", "sha256:1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		name := approve.DefaultName(review(long, value))
		if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
			t.Fatalf("DefaultName(%q) = %q, which is not a name: %v", value, name, problems)
		}
		if other, ok := seen[name]; ok {
			t.Fatalf("DefaultName gives %q for both %q and %q", name, other, value)
		}
		seen[name] = value
		if again := approve.DefaultName(review(long, value)); again != name {
			t.Fatalf("DefaultName(%q) = %q, then %q", value, name, again)
		}
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
		{"no approval required", func(m *operatorv1alpha1.PtahMigration, _ *operatorv1alpha1.PtahMigrationPlan) {
			m.Status.Conditions[0].Status = metav1.ConditionFalse
		}, "does not require an approval"},
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
			// A closed gate still names the plan the status names, and only
			// a plan that is not stored is missing.
			missing := test.says == "is not the plan"
			if review.Missing != missing || review.PlanUID != "uid-p-14" {
				t.Fatalf("Load() missing = %t, plan UID %q; want missing %t and the UID the status names", review.Missing, review.PlanUID, missing)
			}
			if missing && (review.Fingerprint != "" || review.MigrationPlan != nil) {
				t.Fatalf("Load() carries fingerprint %q and plan %v of an object the status does not name", review.Fingerprint, review.MigrationPlan)
			}
		})
	}
}

// The operator replaced the plan under its name after the review: the status
// still names the old UID, and the object stored there is another. Nothing
// may be sent, and the reason is the closed gate, not a changed fingerprint
// read off the other object.
func TestSubmitSendsNothingForAMigrationPlanThatIsNotStored(t *testing.T) {
	t.Parallel()
	objects := waitingMigration(nil)
	api := &answering{Client: planreadtest.NewClient(t, objects...)}
	review, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	stored := &operatorv1alpha1.PtahMigrationPlan{}
	if err := api.Get(context.Background(), client.ObjectKey{Namespace: "application", Name: "shipments-p-14"}, stored); err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	if err := api.Delete(context.Background(), stored); err != nil {
		t.Fatalf("delete the plan: %v", err)
	}
	stored.ResourceVersion, stored.UID, stored.Spec.Fingerprint = "", "uid-p-14-again", "sha256:another"
	if err := api.Client.Create(context.Background(), stored); err != nil {
		t.Fatalf("store another plan under its name: %v", err)
	}

	result := approve.Submit(context.Background(), api, review, approve.Options{})

	if result.Outcome != approve.Closed || !strings.Contains(result.Message, "is not the plan") || len(api.created) != 0 {
		t.Fatalf("Submit() = %+v after %d creates, want closed and nothing sent", result, len(api.created))
	}
}

func TestRenderReviewShowsAMigrationPlanAndWhereItsSQLIs(t *testing.T) {
	t.Parallel()
	const artifact = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	const content = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const policy = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	migrations := []operatorv1alpha1.PlannedMigration{
		{Version: 14, Description: "add shipments", Checksum: "sha256:c14", TransactionMode: "none"},
		{Version: 15, VersionKey: "15b", Description: "backfill\x1b[2Jcarriers", Checksum: "sha256:c15", Checkpoint: true},
	}
	for _, test := range []struct {
		name   string
		change func(*operatorv1alpha1.PtahMigrationPlanSpec)
		says   []string
	}{
		{"an OCI artifact", func(s *operatorv1alpha1.PtahMigrationPlanSpec) { s.ArtifactDigest = artifact },
			[]string{"Source:          OCI artifact", "Artifact:        " + artifact}},
		{"a ConfigMap under a source policy", func(s *operatorv1alpha1.PtahMigrationPlanSpec) {
			s.ContractVersion = 2
			s.Source = &operatorv1alpha1.PlanSourceBinding{Kind: operatorv1alpha1.SourceKindConfigMap, Revision: "uid-orders-migrations",
				ContentDigest: content, Policy: &operatorv1alpha1.SourcePolicyBinding{UID: "uid-policy", Digest: policy}}
		}, []string{"Source:          ConfigMap", "Revision:        uid-orders-migrations", "Content:         " + content, "Source policy:   " + policy}},
		{"nothing recorded", func(*operatorv1alpha1.PtahMigrationPlanSpec) {}, []string{"Source:          none recorded"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			api := planreadtest.NewClient(t, waitingMigration(func(_ *operatorv1alpha1.PtahMigration, p *operatorv1alpha1.PtahMigrationPlan) {
				p.Spec.CurrentVersion = 13
				p.Spec.Migrations = migrations
				test.change(&p.Spec)
			})...)
			review, err := approve.Load(context.Background(), api, approve.Migration, "application", "shipments")
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			var out strings.Builder

			if err := approve.RenderReview(&out, review); err != nil {
				t.Fatalf("RenderReview() error = %v", err)
			}

			text := out.String()
			for _, want := range append([]string{
				"PtahMigration: application/shipments, UID uid-shipments",
				"Plan:          shipments-p-14, UID uid-p-14",
				"Fingerprint:   sha256:14",
				"From version 13, in this order:",
				"14               add shipments", "checksum sha256:c14, transaction none",
				"15b              backfill\\x1b[2Jcarriers", "checksum sha256:c15, checkpoint",
				"Its SQL is read from the\nsource the plan binds, and it is not shown here:",
			}, test.says...) {
				if !strings.Contains(text, want) {
					t.Fatalf("RenderReview() wrote\n%s\nmissing %q", text, want)
				}
			}
			if strings.ContainsRune(text, 0x1b) {
				t.Fatalf("RenderReview() passed a control character through:\n%q", text)
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
