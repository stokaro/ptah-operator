package approve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
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
	// Refused means admission or the API schema refused the approval, in its
	// own words.
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
	// account of why nothing was sent.
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
}

// Manifest is the approval of the reviewed plan, as Submit would send it.
// The approver and the time are left empty: admission stamps them from the
// identity that creates the object, and a value written here is refused.
func Manifest(review Review, name string) client.Object {
	meta := metav1.ObjectMeta{Namespace: review.Namespace, Name: name}
	if name == "" {
		meta.GenerateName = generatedPrefix(review.Name)
	}
	plan := operatorv1alpha1.ImmutableObjectReference{Name: review.PlanName, UID: review.PlanUID}
	owner := operatorv1alpha1.ImmutableObjectReference{Name: review.Name, UID: review.ResourceUID}
	switch review.Family {
	case Migration:
		approval := &operatorv1alpha1.PtahMigrationApproval{
			ObjectMeta: meta,
			Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
				MigrationRef: owner, PlanRef: plan, PlanFingerprint: review.Fingerprint,
			},
		}
		approval.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind("PtahMigrationApproval"))
		return approval
	default:
		approval := &operatorv1alpha1.PtahSchemaApproval{
			ObjectMeta: meta,
			Spec: operatorv1alpha1.PtahSchemaApprovalSpec{
				SchemaRef: owner, PlanRef: plan, PlanFingerprint: review.Fingerprint,
			},
		}
		approval.SetGroupVersionKind(operatorv1alpha1.GroupVersion.WithKind("PtahSchemaApproval"))
		return approval
	}
}

// generatedPrefix keeps a generated name within the 253 characters a name
// may have: the API server appends five characters to the prefix.
func generatedPrefix(resource string) string {
	const room = 253 - 5 - 1
	if len(resource) > room {
		resource = resource[:room]
	}
	return resource + "-"
}

// Submit creates the approval of the reviewed plan, if the cluster still
// names that plan and the operator would admit an approval now.
//
// It reads the resource again first, by identity: a stored plan is immutable,
// so the same resource UID, plan UID and fingerprint are the plan that was
// verified and shown. A plan replaced while the person read the old one, a
// resource deleted and created again, or a resource the operator started
// reading again are all answered without sending anything:
// an approval is a decision about the plan that was shown, and no retry may
// carry it over to another one.
func Submit(ctx context.Context, c client.Client, review Review, opts Options) Result {
	identify := identifySchema
	if review.Family == Migration {
		identify = identifyMigration
	}
	current, err := identify(ctx, c, review.Namespace, review.Name)
	switch {
	case errors.Is(err, ErrNotFound):
		return Result{Outcome: Changed, Message: fmt.Sprintf(
			"%s/%s was deleted after you reviewed its plan. Nothing was approved.", review.Family, review.Name)}
	case errors.Is(err, ErrNothingToApprove):
		return Result{Outcome: Changed, Message: fmt.Sprintf(
			"%s/%s has no current plan any more. Nothing was approved.", review.Family, review.Name)}
	case err != nil:
		return Result{Outcome: Failed, Message: fmt.Sprintf("Nothing was approved: %v", err)}
	}
	if why := changedSince(review, current); why != "" {
		return Result{Outcome: Changed, Message: why + " Nothing was approved; run the command again to review the plan that is current now."}
	}
	if !current.Open {
		return Result{Outcome: Closed, Message: current.Closed + " Nothing was approved."}
	}

	approval := Manifest(review, opts.Name)
	var create []client.CreateOption
	if opts.ServerDryRun {
		create = append(create, client.DryRunAll)
	}
	if err := c.Create(ctx, approval, create...); err != nil {
		outcome, message := classify(err)
		return Result{Outcome: outcome, Message: message}
	}
	if opts.ServerDryRun {
		return Result{Outcome: Previewed, Approval: approval}
	}
	return Result{Outcome: Admitted, Approval: approval}
}

// changedSince names what makes current another plan than the one reviewed,
// or returns "" when it is the same plan of the same resource.
func changedSince(reviewed, current Review) string {
	switch {
	case current.ResourceUID != reviewed.ResourceUID:
		return fmt.Sprintf("%s/%s was deleted and created again after you reviewed its plan.", reviewed.Family, reviewed.Name)
	case current.PlanName != reviewed.PlanName || current.PlanUID != reviewed.PlanUID:
		return fmt.Sprintf("A newer plan, %s, replaced %s while you reviewed it.", current.PlanName, reviewed.PlanName)
	case current.Fingerprint != reviewed.Fingerprint:
		return fmt.Sprintf("Plan %s now carries fingerprint %s, not the %s you reviewed.", current.PlanName, current.Fingerprint, reviewed.Fingerprint)
	}
	return ""
}

// classify turns the API server's answer to the create into an outcome. The
// API server's and admission's messages are passed on unchanged: they are
// the answer this identity gets from kubectl too.
func classify(err error) (Outcome, string) {
	var status apierrors.APIStatus
	switch {
	case errors.As(err, &status) && apierrors.IsForbidden(err) && rbacRefusal.MatchString(status.Status().Message):
		return Forbidden, status.Status().Message
	case errors.As(err, &status) && (apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)):
		return Refused, status.Status().Message
	default:
		// No answer, or one that does not say whether the approval was
		// stored: a timeout, a dropped connection, a server error.
		return Unknown, unknownMessage(err)
	}
}

// rbacRefusal is the authorizer's refusal, which names the user and the verb
// it may not use. Admission refuses with 403 as well, in its own words.
var rbacRefusal = regexp.MustCompile(`is forbidden: User ".*" cannot [a-z]+ resource "`)

func unknownMessage(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout(),
		apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "The API server did not answer in time. The approval may have been stored; read the resource again before approving again."
	default:
		return fmt.Sprintf("The answer was lost (%v). The approval may have been stored; read the resource again before approving again.", err)
	}
}
