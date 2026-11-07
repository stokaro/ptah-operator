package webhook_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
	"github.com/stokaro/ptah-operator/internal/resultretention"
	"github.com/stokaro/ptah-operator/internal/resultstore"
)

// withDeliveryOff makes the shared validator the one a manager with
// resultDelivery.enabled=false builds: no delivery trust, the same retention
// policy. The records a test publishes first are what a release that had
// delivery on left behind. Serial tests never overlap, so nothing else reads the
// field while it is flipped.
func withDeliveryOff(t *testing.T) {
	t.Helper()
	credentials := controllerWriteValidator.ResultCredentials
	controllerWriteValidator.ResultCredentials = nil
	t.Cleanup(func() { controllerWriteValidator.ResultCredentials = credentials })
}

// A credential record keeps its window with delivery off, as every other record
// does. Its resource has let go of the attempt here, so a check of pins alone
// admits the deletion, which is what a namespace's deletion used to do to it
// minutes after the run.
func TestRecordsKeepTheirWindowWithDeliveryOff(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := publicationPayload(t, identity)
	if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatal(err)
	}
	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	withDeliveryOff(t)

	read := func(name string) *api.PtahResultRecord {
		t.Helper()
		record := &api.PtahResultRecord{}
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: name}, record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	b := identity.Binding
	intentName, _ := resultstore.Name(b)
	records := []*api.PtahResultRecord{read(intentName), read(jobconfig.CredentialName(b.UID, b.OperationID, b.JobName))}
	collection := func(record *api.PtahResultRecord) error {
		return admin.DeleteAllOf(t.Context(), &api.PtahResultRecord{}, client.InNamespace(f.namespace), client.MatchingFields{"metadata.name": record.Name}, client.DryRunAll)
	}

	cleanupClockOffset.Store(int64(20 * time.Minute))
	t.Cleanup(func() { cleanupClockOffset.Store(0) })
	for _, record := range records {
		t.Run(record.Spec.Type+" inside its window", func(t *testing.T) {
			requireDenied(t, admin.Delete(t.Context(), record, client.DryRunAll), controllerWriteWebhook, windowDenial(record))
			requireDenied(t, collection(record), controllerWriteWebhook, windowDenial(record))
		})
	}

	// With no policy at all nothing can authorize a deletion, a credential record
	// included: no check that reads less stands in for the window.
	policy := controllerWriteValidator.ResultCleanup
	controllerWriteValidator.ResultCleanup = nil
	t.Cleanup(func() { controllerWriteValidator.ResultCleanup = policy })
	for _, record := range records {
		t.Run(record.Spec.Type+" with no policy", func(t *testing.T) {
			err := admin.Delete(t.Context(), record, client.DryRunAll)
			requireDenied(t, err, controllerWriteWebhook, "result publication retention has not authorized deletion")
			if strings.Contains(err.Error(), "cannot be deleted before") {
				t.Fatalf("a refusal that is not about time named one: %v", err)
			}
		})
	}
}

// Retirement is the write the collector starts every window with, so with delivery
// off it has to reach the checks that judge it. Only a retirement does: every other
// create still needs the delivery trust this installation does not hold.
func TestRetirementNeedsNoDeliveryTrust(t *testing.T) {
	f, identity, store := publicationFixture(t, true)
	payload := publicationPayload(t, identity)
	if _, err := store.Publish(t.Context(), identity.Binding, payload, publicationDigest(payload)); err != nil {
		t.Fatal(err)
	}
	withDeliveryOff(t)

	intentName, _ := resultstore.Name(identity.Binding)
	intent := &api.PtahResultRecord{}
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: f.namespace, Name: intentName}, intent); err != nil {
		t.Fatal(err)
	}
	retirement, err := resultretention.Record(identity.Binding, resultretention.Source{Name: intent.Name, UID: intent.UID, Type: "intent"}, resultretention.MinimumWindow)
	if err != nil {
		t.Fatal(err)
	}

	// The resource still pins the attempt, so the retirement is refused for that.
	requireDenied(t, store.Client.Create(t.Context(), retirement.DeepCopy(), client.DryRunAll), controllerWriteWebhook, "creation is not authorized for its operation")

	another := &api.PtahResultRecord{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.namespace, Name: intentName + "-another"},
		Spec:       intent.Spec,
	}
	requireDenied(t, store.Client.Create(t.Context(), another, client.DryRunAll), controllerWriteWebhook, "result publication trust is not configured")

	f.schema.Status.ActiveOperation = nil
	writeStatus(t, f.schema)
	if err := store.Client.Create(t.Context(), retirement.DeepCopy(), client.DryRunAll); err != nil {
		t.Fatalf("a retirement the resource no longer pins was refused: %v", err)
	}
}
