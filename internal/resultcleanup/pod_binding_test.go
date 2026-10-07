package resultcleanup

import (
	"testing"
	"time"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPublicPodBindingRetainsActiveAuthorityAndCollectsAfterWindow(t *testing.T) {
	f := resulttest.New(t, "schema-plan-dev-fence-scheduling")
	c := &observedClient{Client: f.Client(t), now: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)}
	pins := resultcredentials.PodBindings{Writer: c, Reader: c}
	credential, err := pins.Ensure(t.Context(), f.Identity)
	if err != nil {
		t.Fatal(err)
	}
	root := &api.PtahResultRecord{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: f.Job.Namespace, Name: credential.Name}, root); err != nil {
		t.Fatal(err)
	}
	if binding, err := RootBinding(root); err != nil || binding != f.Identity.Binding {
		t.Fatalf("collector cannot read the public first-Pod pin: %v", err)
	}
	p := Policy{Reader: c, Window: time.Hour, Now: func() time.Time { return c.now }}
	c.admission = &p
	collector, err := New(c, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(2 * time.Hour)
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.AuthorizeDelete(t.Context(), root); err == nil || len(c.deletes) != 0 {
		t.Fatal("an old but active Pod pin became disposable")
	}
	subject := &api.PtahSchema{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(f.Subject), subject); err != nil {
		t.Fatal(err)
	}
	subject.Status.ActiveOperation = nil
	if err := c.Update(t.Context(), subject); err != nil {
		t.Fatal(err)
	}
	if err := c.Client.Delete(t.Context(), f.Job); err != nil {
		t.Fatal(err)
	}
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(c.deletes) != 0 || len(remaining(t, c)) != 2 {
		t.Fatal("retirement did not retain the pin and its new fence for a full window")
	}
	c.now = c.now.Add(time.Hour)
	if err := collector.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(remaining(t, c)) != 0 || len(c.deletes) != 2 || c.deletes[0] != "credential" || c.deletes[1] != "retired" {
		t.Fatalf("eligible pin was not collected with UID-safe deletion and fence last: %v", c.deletes)
	}
}
