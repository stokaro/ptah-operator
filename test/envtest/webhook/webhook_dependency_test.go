package webhook_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The rows above could pass for the wrong reason: a refusal that came from
// somewhere other than the manager's handler, or a Job that nothing would
// have admitted anyway. This row takes the chart's controller-write entry out
// of the installed configuration and shows the refused Job goes through, then
// puts the entry back and shows the refusal returns.
//
// It edits configuration every other row depends on, so it lives in the last
// file of the package and nothing in the package runs beside it: no top-level
// test here is parallel.
func TestControllerWriteRefusalComesFromTheChartEntry(t *testing.T) {
	plane.Require(t)
	ctx := context.Background()
	fixture := newDispatchFixture(t, "dependency")
	managerAPI := clientAs(t, manager.username)
	attempt := func() error { return managerAPI.Create(ctx, fixture.tampered(), client.DryRunAll) }

	requireDenied(t, attempt(), controllerWriteWebhook, "Job is outside the active operation intent")

	removed, index := removeValidatingEntry(t, controllerWriteWebhook)
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		insertValidatingEntry(t, removed, index)
	}
	t.Cleanup(restore)

	// The API server picks webhook configuration changes up from an informer,
	// so the verdict flips shortly after the write rather than with it.
	if err := eventually(10*time.Second, func() error {
		if err := attempt(); err != nil {
			return fmt.Errorf("still refused: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("with %s removed from %s, the manager's tampered Job %s/%s was never admitted: %v",
			controllerWriteWebhook, validatingConfigurationName, fixture.namespace, fixture.job.Name, err)
	}

	restore()
	if err := eventually(10*time.Second, func() error {
		err := attempt()
		if err == nil {
			return fmt.Errorf("still admitted")
		}
		return nil
	}); err != nil {
		t.Fatalf("with %s restored, the manager's tampered Job %s/%s stayed admitted: %v",
			controllerWriteWebhook, fixture.namespace, fixture.job.Name, err)
	}
	requireDenied(t, attempt(), controllerWriteWebhook, "Job is outside the active operation intent")
}

func removeValidatingEntry(t *testing.T, name string) (admissionregistrationv1.ValidatingWebhook, int) {
	t.Helper()
	ctx := context.Background()
	configuration := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := admin.Get(ctx, client.ObjectKey{Name: validatingConfigurationName}, configuration); err != nil {
		t.Fatalf("read ValidatingWebhookConfiguration %s: %v", validatingConfigurationName, err)
	}
	index := slices.IndexFunc(configuration.Webhooks, func(entry admissionregistrationv1.ValidatingWebhook) bool {
		return entry.Name == name
	})
	if index < 0 {
		t.Fatalf("ValidatingWebhookConfiguration %s has no entry %s", validatingConfigurationName, name)
	}
	removed := configuration.Webhooks[index]
	configuration.Webhooks = slices.Delete(configuration.Webhooks, index, index+1)
	if err := admin.Update(ctx, configuration); err != nil {
		t.Fatalf("remove %s from ValidatingWebhookConfiguration %s: %v", name, validatingConfigurationName, err)
	}
	return removed, index
}

func insertValidatingEntry(t *testing.T, entry admissionregistrationv1.ValidatingWebhook, index int) {
	t.Helper()
	ctx := context.Background()
	configuration := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	if err := admin.Get(ctx, client.ObjectKey{Name: validatingConfigurationName}, configuration); err != nil {
		t.Fatalf("read ValidatingWebhookConfiguration %s: %v", validatingConfigurationName, err)
	}
	configuration.Webhooks = slices.Insert(configuration.Webhooks, min(index, len(configuration.Webhooks)), entry)
	if err := admin.Update(ctx, configuration); err != nil {
		t.Fatalf("restore %s to ValidatingWebhookConfiguration %s: %v", entry.Name, validatingConfigurationName, err)
	}
}

// eventually retries check until it returns nil or the timeout passes, and
// then returns the last thing it said.
func eventually(timeout time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
