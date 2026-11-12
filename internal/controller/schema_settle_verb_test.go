package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/operate"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// kubectl ptah settle reads the value from the stop's message rather than
// working it out, so its reader has to read what movedTargetFailure prints, at
// the longest names too, and whatever the annotation holds.
func TestTheSettleVerbReadsTheValueTheStopPrints(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		longest bool
		held    *string
		// clause is what the stop says about the annotation, ahead of the command.
		clause string
	}{
		{name: "nothing written"},
		{name: "another value written", held: ptr("apply-operation"), clause: "holds another value"},
		{name: "the printed value written", held: ptr(movedValue()), clause: "already holds this value"},
		{name: "the longest names", longest: true, held: ptr("another value"), clause: "holds another value"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			schema := safetyPostApplyObserveSchema(t)
			if row.longest {
				schema.Name, schema.Namespace = strings.Repeat("n", 253), strings.Repeat("s", 63)
				schema.Status.PendingObservation.ApplyOperationID = "sha256:" + strings.Repeat("a", 64)
			}
			if row.held != nil {
				schema.Annotations = map[string]string{operatorv1alpha1.SettledApplyAnnotation: *row.held}
			}
			setFailure(schema, operatorv1alpha1.ReasonTargetIdentityChanged, movedTargetFailure(schema, safetyOtherDigest))
			message := meta.FindStatusCondition(schema.Status.Conditions, operatorv1alpha1.ConditionReconciliationFailed).Message
			if row.clause != "" && !strings.Contains(message, row.clause) {
				t.Fatalf("the stop %q does not say %q, so the row reads another message than it names", message, row.clause)
			}

			printed, err := operate.PrintedSettleValue(message, schema.Name, schema.Namespace)

			if err != nil {
				t.Fatalf("the verb cannot read the stop %q: %v", message, err)
			}
			want := settledApplyValue(schema.Status.PendingObservation.ApplyOperationID, safetyOtherDigest)
			if printed.Value != want || printed.ApplyOperationID != schema.Status.PendingObservation.ApplyOperationID ||
				printed.TargetDigest != safetyOtherDigest {
				t.Fatalf("the verb reads %#v, want the value %q the stop prints", printed, want)
			}
			if _, err := operate.PrintedSettleValue(message, schema.Name+"x", schema.Namespace); err == nil {
				t.Fatal("the verb read the stop of one schema as another's")
			}
		})
	}
}

// What the verb writes is what the printed command writes, so the controller
// takes it the same way: at the next read of the target, and over a value that
// settled nothing. The verb sends nothing while the annotation holds it.
func TestTheSettleVerbsStatementSettlesTheProof(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		held string
	}{
		{name: "no annotation"},
		{name: "over the Apply's ID alone", held: "apply-operation"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			blocked, reconciler, api := blockedProof(t, operatorv1alpha1.PendingObservationApplySucceeded)
			if row.held != "" {
				setSettledApply(t, api, blocked, row.held)
			}
			ref := operate.Ref{Family: operate.FamilySchema, Namespace: blocked.Namespace, Name: blocked.Name}

			result, err := operate.Settle(context.Background(), api, ref)

			if err != nil || !result.Changed || result.Printed.Value != movedValue() {
				t.Fatalf("Settle = %#v, %v; want the printed value written", result, err)
			}
			if held := storedSchemaOf(t, api, blocked).Annotations[operatorv1alpha1.SettledApplyAnnotation]; held != movedValue() {
				t.Fatalf("the annotation holds %q, want %q", held, movedValue())
			}
			again, err := operate.Settle(context.Background(), api, ref)
			if err != nil || again.Changed {
				t.Fatalf("Settle again = %#v, %v; want nothing sent", again, err)
			}
			assertSettled(t, nextProofRead(t, reconciler, api, blocked, "second", safetyOtherDigest, nil))
		})
	}
}

// The verb refuses a resource the controller would take no statement from.
func TestTheSettleVerbRefusesWhatTheControllerWouldNotTake(t *testing.T) {
	t.Parallel()
	t.Run("a proof failing on its own target", func(t *testing.T) {
		t.Parallel()
		failure := &runner.ResultError{Code: "execution_error", Message: "the server went away"}
		schema, reconciler, api := proofFixture(t, operatorv1alpha1.PendingObservationApplySucceeded, nil)
		reconciler.Logs = staticLogs{content: movedTargetFrame(t, schema, testDigest, failure)}
		reconcileSchema(t, reconciler, api, schema)
		_, err := operate.Settle(context.Background(), api, operate.Ref{Family: operate.FamilySchema, Namespace: schema.Namespace, Name: schema.Name})
		var notStopped *operate.NotStoppedError
		if !errors.As(err, &notStopped) {
			t.Fatalf("Settle = %v, want it refused as not stopped", err)
		}
		if held, written := storedSchemaOf(t, api, schema).Annotations[operatorv1alpha1.SettledApplyAnnotation]; written {
			t.Fatalf("a refused settlement wrote %q", held)
		}
	})
}
