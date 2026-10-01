package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func soakScenarios() *scenarios {
	s := twoNamespaceScenarios()
	s.in.catalog = testInputCatalog()
	s.load.Interval = duration{2 * time.Minute}
	s.load.Settle = duration{5 * time.Minute}
	s.load.SteadyState = duration{90 * time.Minute}
	s.load.SampleEvery = duration{5 * time.Second}
	s.load.ChangeBatch = 5
	s.load.Soak = &soakWorkload{Rounds: 9, Cadence: duration{10 * time.Minute}, ChurnPerFamily: 2, MinimumCycles: 30}
	return s
}

func TestSoakWorkloadRefusesMissingOrContradictoryDimensions(t *testing.T) {
	w, err := loadWorkload("../../support/capacity/soak.json")
	if err != nil {
		t.Fatal(err)
	}
	if w.Soak.Rounds != 9 || w.Soak.ChurnPerFamily != 2 || w.Soak.MinimumCycles != 30 || w.Soak.Cadence.Duration != 10*time.Minute || w.SteadyState.Duration != 90*time.Minute {
		t.Fatal("frozen soak dimensions changed")
	}
	for name, change := range map[string]func(*workload){
		"no rounds": func(w *workload) { w.Soak.Rounds = 0 }, "unprepared round": func(w *workload) { w.Soak.Rounds = 10 },
		"no cadence": func(w *workload) { w.Soak.Cadence.Duration = 0 }, "no churn": func(w *workload) { w.Soak.ChurnPerFamily = 0 },
		"changed slot churn": func(w *workload) { w.Soak.ChurnPerFamily = 6 }, "no cycles": func(w *workload) { w.Soak.MinimumCycles = 0 },
		"wrong duration": func(w *workload) { w.SteadyState.Duration = time.Minute }, "wrong population": func(w *workload) { w.Schemas = 9 },
		"wrong update count": func(w *workload) { w.ChangeBatch = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := w
			config := *w.Soak
			candidate.Soak = &config
			change(&candidate)
			if candidate.validate() == nil {
				t.Fatal("invalid soak accepted")
			}
		})
	}
	raw, err := os.ReadFile("../../support/capacity/soak.json")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "workload.json")
	if err := os.WriteFile(p, append(raw, []byte("{}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkload(p); err == nil {
		t.Fatal("trailing configuration accepted")
	}
}

// Use the retained native convergence condition values, not a phase-only
// synthetic success. Identity and spec here belong to the isolated unit fixture.
func soakObject(t *testing.T, s *scenarios, family string, index int) *unstructured.Unstructured {
	t.Helper()
	_, f := restartReplayFixture(t)
	var accepted *cycleReading
	for _, h := range f.Histories {
		if h.Family == family {
			for i := range h.Readings {
				r := &h.Readings[i]
				if cycleReady(family, *r) {
					accepted = r
					break
				}
			}
		}
		if accepted != nil {
			break
		}
	}
	if accepted == nil {
		t.Fatal("native fixture has no converged reading")
	}
	obj := s.schemaObject(index)
	if family == "migration" {
		obj = s.migrationObject(s.migrationName(index), 10+index, "Always", true)
	}
	obj.SetUID(types.UID(family + fmt.Sprint(index)))
	obj.SetResourceVersion("17")
	obj.SetGeneration(1)
	obj.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
	conditions := append([]metav1.Condition(nil), accepted.Conditions...)
	for i := range conditions {
		conditions[i].ObservedGeneration = 1
	}
	raw, _ := json.Marshal(conditions)
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	status := map[string]any{"observedGeneration": int64(1), "conditions": values}
	if family == "schema" {
		status["lastSuccessfulReconciliation"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		status["source"] = map[string]any{"digest": digestOf(s.schemaReference(index, 0))}
	} else {
		status["history"] = map[string]any{"observedAt": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}
		status["artifact"] = map[string]any{"digest": digestOf(s.migrationReference(index, 0))}
	}
	obj.Object["status"] = status
	return obj
}

func TestChurnCopiesOnlyCurrentSpecAndRefusesUnsafeResources(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			s := soakScenarios()
			original := soakObject(t, s, family, 8)
			original.SetFinalizers([]string{"operator.ptah.run/cleanup"})
			original.SetAnnotations(map[string]string{"old": "state"})
			replacement, err := churnReplacement(original, family, s.load.Name)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.GetUID() != "" || replacement.GetResourceVersion() != "" || replacement.GetGeneration() != 0 || replacement.Object["status"] != nil || len(replacement.GetFinalizers()) > 0 || len(replacement.GetAnnotations()) > 0 || !reflect.DeepEqual(replacement.Object["spec"], original.Object["spec"]) {
				t.Fatalf("replacement copied runtime state or changed spec: %v", replacement)
			}
			for name, mutate := range map[string]func(*unstructured.Unstructured){
				"claim": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedMap(o.Object, map[string]any{"type": "Plan", "id": "claim", "startedAt": time.Now().UTC().Format(time.RFC3339)}, "status", "activeOperation")
				},
				"unresolved annotation": func(o *unstructured.Unstructured) {
					o.SetAnnotations(map[string]string{operatorv1alpha1.UnresolvedRunAnnotation: "{}"})
				},
				"unresolved status": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "status", "unresolvedRun")
				},
				"pending release": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "status", "pendingLockRelease")
				},
				"pending binding": func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "status", "pendingBindingRetirement")
				},
				"stale generation": func(o *unstructured.Unstructured) { o.SetGeneration(2) },
				"no RV":            func(o *unstructured.Unstructured) { o.SetResourceVersion("") },
				"not ours":         func(o *unstructured.Unstructured) { o.SetLabels(nil) },
				"deleting":         func(o *unstructured.Unstructured) { v := metav1.Now(); o.SetDeletionTimestamp(&v) },
				"only phase":       func(o *unstructured.Unstructured) { o.Object["status"] = map[string]any{"phase": "InSync"} },
			} {
				t.Run(name, func(t *testing.T) {
					bad := original.DeepCopy()
					mutate(bad)
					if _, err := churnReplacement(bad, family, s.load.Name); err == nil {
						t.Fatal("unsafe churn accepted")
					}
				})
			}
		})
	}
}

func TestBatchConvergenceRequiresOriginalIdentityAndFreshDatabaseState(t *testing.T) {
	for _, family := range []string{"schema", "migration"} {
		t.Run(family, func(t *testing.T) {
			s := soakScenarios()
			object := soakObject(t, s, family, 0)
			ref := s.schemaReference(0, 0)
			if family == "migration" {
				ref = s.migrationReference(0, 0)
			}
			target := batchTarget{family: family, namespace: object.GetNamespace(), name: object.GetName(), uid: object.GetUID(), generation: 1, reference: ref, applied: family == "schema"}
			if family == "schema" {
				_ = unstructured.SetNestedMap(object.Object, map[string]any{"artifactDigest": digestOf(ref)}, "status", "applied")
			}
			before := time.Now().Add(-2 * time.Minute)
			if ok, err := targetConverged(object, target, before); err != nil || !ok {
				t.Fatal(ok, err)
			}
			cases := []string{"UID", "generation", "namespace", "name", "old read", "future read", "artifact"}
			if family == "schema" {
				cases = append(cases, "applied")
			}
			for _, name := range cases {
				t.Run(name, func(t *testing.T) {
					bad := object.DeepCopy()
					after := before
					switch name {
					case "UID":
						bad.SetUID("replacement")
					case "generation":
						bad.SetGeneration(2)
					case "namespace":
						bad.SetNamespace("other")
					case "name":
						bad.SetName("other")
					case "old read":
						after = time.Now()
					case "future read":
						if family == "schema" {
							_ = unstructured.SetNestedField(bad.Object, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "status", "lastSuccessfulReconciliation")
						} else {
							_ = unstructured.SetNestedField(bad.Object, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "status", "history", "observedAt")
						}
					case "artifact":
						target.reference = "oci://other@sha256:" + strings.Repeat("f", 64)
						defer func() { target.reference = ref }()
					case "applied":
						unstructured.RemoveNestedField(bad.Object, "status", "applied")
					}
					if ok, err := targetConverged(bad, target, after); err == nil && ok {
						t.Fatal("unproven convergence accepted")
					}
				})
			}
		})
	}
}

func TestExpiredSoakRoundCannotMutateOrInvokeProbe(t *testing.T) {
	s := soakScenarios()
	called := false
	s.checkpoint = func(context.Context, int, string) (databaseCheckpoint, error) {
		called = true
		return databaseCheckpoint{}, nil
	}
	if err := s.soakRound(t.Context(), 1, time.Now().Add(-time.Second)); err == nil || called {
		t.Fatal("expired round ran work", called, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitUntil(ctx, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("canceled schedule waited")
	}
}
