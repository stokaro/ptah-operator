package crd_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nameRule = "oldSelf.hasValue() || self.metadata.name.size() <= 63"

var crdKind = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// The name rule carries optionalOldSelf, so it binds a create and admits every
// update of an object already stored, whatever its name. A cluster can hold a
// longer name -- one stored before the rule shipped -- and the rule must not
// strand it: the controller has to keep writing its finalizer and status.
//
// The API server will not store such a name while the rule is in force, so the
// row takes the rule out of the installed CRD, stores a 64-byte name, puts the
// CRD back exactly as it was, and then updates the stored object. It edits a
// CRD every other test reads, which is why it is not parallel: Go runs it to
// completion before any parallel test resumes.
func TestTheNameRuleBindsTheCreateAndNotTheUpdate(t *testing.T) {
	plane.Require(t)

	for _, family := range []struct {
		crd  string
		base func(namespace string) func() *unstructured.Unstructured
	}{
		{crd: "ptahschemas.operator.ptah.run", base: schemaBase},
		{crd: "ptahmigrations.operator.ptah.run", base: migrationBase},
	} {
		t.Run(family.crd, func(t *testing.T) {
			namespace := newNamespace(t, "name-rule")
			stored := family.base(namespace)()
			stored.SetName(longName)
			probe := family.base(namespace)()
			probe.SetName(longName[:63] + "b")

			refusedAgain := func(context.Context) (bool, error) {
				return refusalMismatch(dryRunCreate(probe), nameLengthCause) == nil, nil
			}
			if ok, _ := refusedAgain(context.Background()); !ok {
				t.Fatalf("a 64-byte name is admitted before the rule was touched: %v",
					refusalMismatch(dryRunCreate(probe), nameLengthCause))
			}

			original := readCRD(t, family.crd)
			restore := func() error {
				current := readCRD(t, family.crd)
				current.Object["spec"] = original.DeepCopy().Object["spec"]
				return api.Update(context.Background(), current)
			}
			relaxed := readCRD(t, family.crd)
			if err := dropRootRule(relaxed, nameRule); err != nil {
				t.Fatalf("%s: %v", family.crd, err)
			}
			if err := api.Update(context.Background(), relaxed); err != nil {
				t.Fatalf("drop the name rule from %s: %v", family.crd, err)
			}
			restored := false
			t.Cleanup(func() {
				if !restored {
					if err := restore(); err != nil {
						t.Errorf("put the name rule back on %s: %v", family.crd, err)
					}
				}
			})

			admitted := func(context.Context) (bool, error) { return dryRunCreate(stored) == nil, nil }
			if err := poll(admitted); err != nil {
				t.Fatalf("%s still refuses a 64-byte name after the rule was dropped: %v; last answer: %v",
					family.crd, err, dryRunCreate(stored))
			}
			if err := api.Create(context.Background(), stored); err != nil {
				t.Fatalf("store %s %s without the rule: %v", stored.GetKind(), stored.GetName(), err)
			}

			if err := restore(); err != nil {
				t.Fatalf("put the name rule back on %s: %v", family.crd, err)
			}
			restored = true
			if err := poll(refusedAgain); err != nil {
				t.Fatalf("%s admits a new 64-byte name after the rule was restored: %v; last answer: %v",
					family.crd, err, refusalMismatch(dryRunCreate(probe), nameLengthCause))
			}

			updated := reread(t, stored)
			set(t, updated, "20m", "spec", "interval")
			if err := api.Update(context.Background(), updated); err != nil {
				t.Fatalf("the name rule refused an update of the stored %s %s: %v", stored.GetKind(), stored.GetName(), err)
			}
			if interval, _, _ := unstructured.NestedString(reread(t, stored).Object, "spec", "interval"); interval != "20m" {
				t.Fatalf("%s %s stored interval %q after the update, want 20m", stored.GetKind(), stored.GetName(), interval)
			}
			// The same answer on either side of the update: the rule was in
			// force when the long name was written to.
			if mismatch := refusalMismatch(dryRunCreate(probe), nameLengthCause); mismatch != nil {
				t.Fatalf("a new 64-byte %s after the update: %v", stored.GetKind(), mismatch)
			}
		})
	}
}

func readCRD(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdKind)
	if err := api.Get(context.Background(), client.ObjectKey{Name: name}, crd); err != nil {
		t.Fatalf("read CRD %s: %v", name, err)
	}
	return crd
}

// dropRootRule removes one rule from the root schema of every served version,
// and refuses to report success if the rule was not there to remove.
func dropRootRule(crd *unstructured.Unstructured, rule string) error {
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil {
		return err
	}
	dropped := 0
	for index, entry := range versions {
		version, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("version %d is not an object", index)
		}
		rules, _, err := unstructured.NestedSlice(version, "schema", "openAPIV3Schema", "x-kubernetes-validations")
		if err != nil {
			return err
		}
		kept := make([]any, 0, len(rules))
		for _, candidate := range rules {
			if validation, ok := candidate.(map[string]any); ok && validation["rule"] == rule {
				dropped++
				continue
			}
			kept = append(kept, candidate)
		}
		if err := unstructured.SetNestedSlice(version, kept, "schema", "openAPIV3Schema", "x-kubernetes-validations"); err != nil {
			return err
		}
		versions[index] = version
	}
	if dropped == 0 {
		return errors.New("the CRD carries no root rule " + rule)
	}
	return unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")
}

// poll waits for the API server to serve a CRD change. It rebuilds the
// resource's handler asynchronously, so the first request after an update can
// still see the old schema.
func poll(condition wait.ConditionWithContextFunc) error {
	return wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 30*time.Second, true, condition)
}
