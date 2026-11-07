package approve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/apirefusal"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Outcome is what became of an approval this package was asked to submit.
type Outcome string

const (
	// Admitted means the API server stored the approval. Admission stamped
	// the approver; the operator decides when the plan runs.
	Admitted Outcome = "admitted"
	// Previewed means admission checked the approval in a server-side dry
	// run and nothing was stored.
	Previewed Outcome = "previewed"
	// Changed means the plan the person reviewed is no longer the current
	// plan, or the resource is another one now. Nothing was sent.
	Changed Outcome = "changed"
	// Closed means the operator would not admit an approval now. Nothing was
	// sent.
	Closed Outcome = "closed"
	// Forbidden means Kubernetes RBAC does not let this identity create the
	// approval.
	Forbidden Outcome = "forbidden"
	// Refused means the API server answered the create with a refusal that
	// is not RBAC's: admission, the API schema, a name already taken, a
	// credential it did not accept, or a request it was too busy to take.
	// Nothing was stored.
	Refused Outcome = "refused"
	// Unknown means the request was sent and no answer said whether the
	// approval was stored.
	Unknown Outcome = "unknown"
	// Failed means the cluster could not be read before anything was sent.
	Failed Outcome = "failed"
)

// Result is the outcome of one submission and what it stored, if anything.
type Result struct {
	Outcome Outcome
	// Message is the API server's or admission's words, or this package's
	// account of why nothing was sent, with what to do next.
	Message string
	// Approval is the object as the API server answered with it, carrying the
	// identity admission stamped: nil unless admitted or previewed.
	Approval client.Object
}

// Approver is the username admission recorded on the approval, if any.
func (r Result) Approver() (string, *metav1.Time) {
	switch approval := r.Approval.(type) {
	case *operatorv1alpha1.PtahSchemaApproval:
		return approval.Spec.Approver.Username, &approval.Spec.ApprovedAt
	case *operatorv1alpha1.PtahMigrationApproval:
		return approval.Spec.Approver.Username, &approval.Spec.ApprovedAt
	default:
		return "", nil
	}
}

// Options shape the approval Submit sends.
type Options struct {
	// Name is the approval's name. Empty asks the API server to generate one
	// from the resource's name.
	Name string
	// ServerDryRun asks admission to check the approval without storing it.
	ServerDryRun bool
	// Await is how long Submit waits for the operator to come back to the
	// approval gate when it finds it reading the resource again
	// (Review.Reading). Zero answers such a read at once, as Closed.
	Await time.Duration
	// CallTimeout bounds each request Submit sends, and zero leaves them to
	// ctx. A wait spans many requests, so it does not bound the wait.
	CallTimeout time.Duration
	// Waiting, when set, is called once when Submit begins to wait, with
	// what the operator is doing.
	Waiting func(why string)
}

// pollInterval is how often Submit reads the resource while it waits for the
// approval gate.
const pollInterval = 2 * time.Second

// Manifest is the approval of the reviewed plan as a repository holds it: the
// API version, the kind, the name and the namespace, and the three references
// that are the decision. Nothing else is written. Admission stamps the
// approver and the time from whoever creates the object, and an empty value
// written here would be sent back by a later apply as an edit of an approval,
// which is immutable.
//
// It is named as Submit names what it sends, by approvalMeta. Without a name
// the API server generates one each time it is created, so one file can approve
// a plan again after an uncertain Apply consumed the first approval. kubectl
// apply refuses a generated name; a workflow that applies its files passes one.
func Manifest(review Review, name string) *unstructured.Unstructured {
	meta := approvalMeta(review, name)
	metadata := map[string]any{"namespace": meta.Namespace}
	if meta.Name != "" {
		metadata["name"] = meta.Name
	} else {
		metadata["generateName"] = meta.GenerateName
	}
	owner := "schemaRef"
	if review.Family == Migration {
		owner = "migrationRef"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": operatorv1alpha1.GroupVersion.String(),
		"kind":       review.Family.ApprovalKind(),
		"metadata":   metadata,
		"spec": map[string]any{
			owner:             map[string]any{"name": review.Name, "uid": string(review.ResourceUID)},
			"planRef":         map[string]any{"name": review.PlanName, "uid": string(review.PlanUID)},
			"planFingerprint": review.Fingerprint,
		},
	}}
}

// approvalMeta names an approval, printed or sent: as given, or by the API
// server from the resource's name. The API server cuts a long base itself.
func approvalMeta(review Review, name string) metav1.ObjectMeta {
	meta := metav1.ObjectMeta{Namespace: review.Namespace, Name: name}
	if name == "" {
		meta.GenerateName = review.Name + "-"
	}
	return meta
}

// approvalObject is the approval Submit sends.
func approvalObject(review Review, name string) client.Object {
	meta := approvalMeta(review, name)
	plan := operatorv1alpha1.ImmutableObjectReference{Name: review.PlanName, UID: review.PlanUID}
	owner := operatorv1alpha1.ImmutableObjectReference{Name: review.Name, UID: review.ResourceUID}
	var approval client.Object
	switch review.Family {
	case Migration:
		approval = &operatorv1alpha1.PtahMigrationApproval{
			ObjectMeta: meta,
			Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
				MigrationRef: owner, PlanRef: plan, PlanFingerprint: review.Fingerprint,
			},
		}
	default:
		approval = &operatorv1alpha1.PtahSchemaApproval{
			ObjectMeta: meta,
			Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
				SchemaRef: owner, PlanRef: plan, PlanFingerprint: review.Fingerprint,
			},
		}
	}
	setKind(approval, review.Family)
	return approval
}

func setKind(approval client.Object, family Family) {
	approval.GetObjectKind().SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind(family.ApprovalKind()))
}

// Submit creates the approval of the reviewed plan, if the cluster still
// names that plan and the operator would admit an approval now.
//
// It reads the resource again first, by identity: a stored plan is immutable,
// so the same resource UID, plan UID and fingerprint are the plan that was
// verified and shown. A plan replaced while the person read the old one, a
// resource deleted and created again, or a spec changed since are all
// answered without sending anything: an approval is a decision about the plan
// that was shown, under the spec it was shown under, and no retry may carry
// it over to another one.
//
// A resource the operator is reading again closes the gate only until that
// read ends, and admission refuses an approval meanwhile. With opts.Await
// Submit waits for the read to end, reading the resource at pollInterval and
// holding every read to the same identity, and sends the approval once the
// gate is open with the reviewed plan. A read that ends anywhere else is
// answered as it would have been at once.
func Submit(ctx context.Context, c client.Client, review Review, opts Options) Result {
	identify := identifySchema
	if review.Family == Migration {
		identify = identifyMigration
	}
	gate := gateWait{review: review, opts: opts}
	for {
		readCtx, cancel := bounded(ctx, opts.CallTimeout)
		current, err := identify(readCtx, c, review.Namespace, review.Name)
		cancel()
		switch {
		case errors.Is(err, ErrNotFound):
			return Result{Outcome: Changed, Message: fmt.Sprintf(
				"%s/%s was deleted after you reviewed its plan. Nothing was approved.", review.Family, review.Name)}
		case err != nil && !errors.Is(err, ErrNothingToApprove):
			// An answer this plugin cannot use is not a read that failed on
			// the way: reading again changes nothing. The status it came with
			// may already name another plan, which is the answer to give.
			var local unreadable
			if errors.As(err, &local) {
				if current.ResourceUID != "" {
					if why := changedSince(review, current); why != "" {
						return Result{Outcome: Changed, Message: why + " Nothing was approved; run the command again to review the plan that is current now."}
					}
				}
				return Result{Outcome: Failed, Message: fmt.Sprintf("Nothing was approved: %v", err)}
			}
			// Once Submit waits, a read the API server may answer next time is
			// read again until the wait is over; before, nothing was promised.
			if gate.deadline.IsZero() || !operate.Retryable(err) {
				return Result{Outcome: Failed, Message: fmt.Sprintf("Nothing was approved: %v", err)}
			}
			gate.lastErr = err
			if result, over := gate.pause(ctx); over {
				return result
			}
			continue
		}
		gate.lastErr = nil
		if why := changedSince(review, current); why != "" {
			return Result{Outcome: Changed, Message: why + " Nothing was approved; run the command again to review the plan that is current now."}
		}
		if err != nil && !current.Reading {
			return Result{Outcome: Changed, Message: fmt.Sprintf(
				"%s/%s has no current plan any more. Nothing was approved.", review.Family, review.Name)}
		}
		if current.Reading {
			why := current.Closed
			if why == "" {
				// The read cleared the plan, so no gate was judged.
				why = fmt.Sprintf("The operator is reading %s/%s again, and publishes its plan when that read ends.", review.Family, review.Name)
			}
			if opts.Await <= 0 {
				return Result{Outcome: Closed, Message: why + " Nothing was approved."}
			}
			if gate.deadline.IsZero() {
				gate.deadline = time.Now().Add(opts.Await)
				if opts.Waiting != nil {
					opts.Waiting(why)
				}
			}
			if result, over := gate.pause(ctx); over {
				return result
			}
			continue
		}
		if !current.Open {
			return Result{Outcome: Closed, Message: current.Closed + " Nothing was approved."}
		}
		break
	}

	approval := approvalObject(review, opts.Name)
	var create []client.CreateOption
	if opts.ServerDryRun {
		create = append(create, client.DryRunAll)
	}
	createCtx, cancel := bounded(ctx, opts.CallTimeout)
	defer cancel()
	if err := c.Create(createCtx, approval, create...); err != nil {
		return refusal(createCtx, c, review, opts.Name, err)
	}
	// The typed client decodes the answer without its kind. What is printed
	// is the object as the API server holds it, kind included.
	setKind(approval, review.Family)
	if opts.ServerDryRun {
		return Result{Outcome: Previewed, Approval: approval}
	}
	return Result{Outcome: Admitted, Approval: approval}
}

// gateWait is Submit waiting for the operator to end a read of the resource.
type gateWait struct {
	review   Review
	opts     Options
	deadline time.Time
	// lastErr is the last read's failure, when the API server may answer the
	// next one.
	lastErr error
}

// pause sleeps until the next read, or ends the wait: at its deadline, or
// when ctx ends.
func (g *gateWait) pause(ctx context.Context) (Result, bool) {
	now := time.Now()
	if !now.Before(g.deadline) {
		if g.lastErr != nil {
			return Result{Outcome: Failed, Message: fmt.Sprintf("No read of %s/%s found the approval gate open within %s, "+
				"and the last one failed: %v. Nothing was approved.", g.review.Family, g.review.Name,
				statusview.Duration(g.opts.Await), g.lastErr)}, true
		}
		return Result{Outcome: Closed, Message: fmt.Sprintf("The operator was still reading %s/%s again after %s. "+
			"Nothing was approved; run the command again once it waits for an approval.", g.review.Family, g.review.Name,
			statusview.Duration(g.opts.Await))}, true
	}
	select {
	case <-ctx.Done():
		return Result{Outcome: Failed, Message: fmt.Sprintf("Nothing was approved: %v", ctx.Err())}, true
	case <-time.After(min(pollInterval, g.deadline.Sub(now))):
		return Result{}, false
	}
}

// bounded is ctx with timeout, or ctx alone when timeout is not positive.
func bounded(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// changedSince names what makes current another plan than the one reviewed,
// or returns "" when it is the same plan of the same resource under the same
// spec. A current review with no plan, read while the operator reads the
// resource again, names no other plan.
func changedSince(reviewed, current Review) string {
	switch {
	case current.ResourceUID != reviewed.ResourceUID:
		return fmt.Sprintf("%s/%s was deleted and created again after you reviewed its plan.", reviewed.Family, reviewed.Name)
	case current.Spec != reviewed.Spec:
		// A suspend or a resume is not a change of the desired state
		// (desiredspec), and moves the generation all the same.
		return fmt.Sprintf("The spec of %s/%s changed after you reviewed its plan.", reviewed.Family, reviewed.Name)
	case current.PlanName == "":
		return ""
	case current.PlanName != reviewed.PlanName || current.PlanUID != reviewed.PlanUID:
		return fmt.Sprintf("A newer plan, %s, replaced %s while you reviewed it.", current.PlanName, reviewed.PlanName)
	case current.Missing:
		// The status names the reviewed plan, and the object under its name
		// is another one. That has no fingerprint to compare; it is closed,
		// and current.Closed says why.
		return ""
	case current.Fingerprint != reviewed.Fingerprint:
		return fmt.Sprintf("Plan %s now carries fingerprint %s, not the %s you reviewed.",
			current.PlanName, statusview.Clean(current.Fingerprint), statusview.Clean(reviewed.Fingerprint))
	}
	return ""
}

// refusal turns the API server's answer to the create into a result. A status
// in the 4xx range is a definite answer: the request was judged and nothing
// was stored. A server error, a timeout or a lost connection is not, whatever
// it says, because the write may have reached storage before the answer was
// lost. The API server's and admission's own words are passed on: they are
// the answer this identity gets from kubectl too.
func refusal(ctx context.Context, c client.Reader, review Review, name string, err error) Result {
	if !apirefusal.Definite(err) {
		return Result{Outcome: Unknown, Message: unknownMessage(err, review)}
	}
	message := err.Error()
	if status := apierrors.APIStatus(nil); errors.As(err, &status) {
		message = status.Status().Message
	}
	switch {
	case apirefusal.ByRBAC(err):
		return Result{Outcome: Forbidden, Message: message + "\nNothing was approved."}
	case apierrors.IsAlreadyExists(err):
		return Result{Outcome: Refused, Message: message + "\n" + taken(ctx, c, review, name)}
	case apierrors.IsUnauthorized(err):
		return Result{Outcome: Refused, Message: message + "\nThe API server did not accept your credentials. Nothing was approved."}
	case apierrors.IsTooManyRequests(err):
		return Result{Outcome: Refused, Message: message + "\nThe API server was too busy to take the request, and nothing was stored. Run the command again."}
	case apierrors.IsNotFound(err):
		return Result{Outcome: Refused, Message: message + "\nNothing was approved: something the approval names is gone. " +
			listsApprovals(review) + " what is current."}
	default:
		return Result{Outcome: Refused, Message: message + "\nNothing was approved."}
	}
}

// taken says what holds the name an approval was refused for. An approval of
// the same plan under that name is the decision already recorded, by whoever
// created it: an earlier run whose answer was lost, or someone else.
func taken(ctx context.Context, c client.Reader, review Review, name string) string {
	// A fresh object: a decode leaves a field the answer omits as it was.
	var existing client.Object = &operatorv1alpha1.PtahSchemaApproval{}
	if review.Family == Migration {
		existing = &operatorv1alpha1.PtahMigrationApproval{}
	}
	if name == "" || c.Get(ctx, client.ObjectKey{Namespace: review.Namespace, Name: name}, existing) != nil {
		return "The name is taken. Nothing was approved; pass another --name."
	}
	var owner, plan operatorv1alpha1.ImmutableObjectReference
	var fingerprint string
	var conditions []metav1.Condition
	switch approval := existing.(type) {
	case *operatorv1alpha1.PtahMigrationApproval:
		owner, plan, fingerprint = approval.Spec.MigrationRef, approval.Spec.PlanRef, approval.Spec.PlanFingerprint
		conditions = approval.Status.Conditions
	case *operatorv1alpha1.PtahSchemaApproval:
		owner, plan, fingerprint = approval.Spec.SchemaRef, approval.Spec.PlanRef, approval.Spec.PlanFingerprint
		conditions = approval.Status.Conditions
	}
	if owner.UID == review.ResourceUID && plan.Name == review.PlanName && plan.UID == review.PlanUID && fingerprint == review.Fingerprint {
		if conditionTrue(conditions, operatorv1alpha1.ConditionApprovalStale) {
			// The same plan can come back after its approval was withdrawn, and
			// the name then holds a decision that authorizes nothing.
			return fmt.Sprintf("%s %s bound plan %s and went stale, so it authorizes nothing, and this request stored nothing. Nothing was approved; pass another --name.",
				review.Family.ApprovalKind(), name, review.PlanName)
		}
		return fmt.Sprintf("%s %s already binds plan %s with fingerprint %s: an approval of this plan is recorded, and this request stored nothing. %s who approved it.",
			review.Family.ApprovalKind(), name, review.PlanName, statusview.Clean(review.Fingerprint), listsApprovals(review))
	}
	return fmt.Sprintf("%s %s binds another plan. Nothing was approved; pass another --name.", review.Family.ApprovalKind(), name)
}

// listsApprovals is the command that shows the approvals naming the resource,
// beginning a sentence.
func listsApprovals(review Review) string {
	return fmt.Sprintf("kubectl ptah %s %s -n %s shows", review.Family, review.Name, review.Namespace)
}

func unknownMessage(err error, review Review) string {
	var netErr net.Error
	lead := fmt.Sprintf("The answer was lost (%v).", err)
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() ||
		apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		lead = "The API server did not answer in time."
	}
	return fmt.Sprintf("%s The approval may have been stored; %s the approvals that name %s before you approve again.",
		lead, listsApprovals(review), review.Name)
}
