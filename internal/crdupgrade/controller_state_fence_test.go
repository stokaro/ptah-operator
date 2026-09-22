package crdupgrade

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// migrationCarryingTheBumpedFields builds a PtahMigration whose durable status
// holds the two records that moved the controller-state version, stamped with
// the version the writer compiled.
//
// A migration records its version once, at the execution binding, and never
// clears it: internal/controller/migration_controller.go assigns
// status.executionBinding and nothing assigns nil back. status.unresolvedRun
// and status.activeOperation.retryNotBefore can only exist on a resource that
// has dispatched, so the binding is already there when they are written, and
// the binding is the location the preflight scans. That is the whole reason
// raising the number covers the two fields without adding a scan path.
func migrationCarryingTheBumpedFields(namespace, name string, stamp int64) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"status": map[string]any{
			"executionBinding": map[string]any{
				"controllerStateVersion": stamp,
				"epoch":                  int64(7),
			},
			// #252: the record that stops a blind replay after an Apply whose
			// outcome nobody established.
			"unresolvedRun": map[string]any{
				"version": "0004",
				"reason":  "ApplyOutcomeUnknown",
			},
			// #262: the delay a retried operation waits out.
			"activeOperation": map[string]any{
				"operation":        "Apply",
				"retryNotBefore":   "2026-09-22T00:00:00Z",
				"attemptsRecorded": int64(1),
			},
		},
	}}
	object.SetUID(types.UID("uid-" + namespace + "-" + name))
	return object
}

func storedStateClientsWithMigrations(migrations ControllerStateListClient) StoredControllerStateClients {
	clients := emptyStoredStateClients()
	clients.Migrations = migrations
	return clients
}

// The downgrade fence has to refuse a manager that predates the two fields,
// and the row that proves the bump was needed is the one where it does not.
//
// Both rows store the same records. Only the stamped version differs, and with
// the number the contract carried before this change the fence is silent: an
// older manager is admitted, retires the claim, and replays an Apply whose
// outcome the record it could not read says nobody established. Reading the
// fence again would not have shown that -- the fence was always correct about
// the number it was given.
func TestTheStoredStateFenceRefusesAManagerThatPredatesTheBumpedFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		stamp       int64
		wantRefusal bool
	}{
		{
			name:        "stamped with the version that covers both fields",
			stamp:       int64(ourStateVersion),
			wantRefusal: true,
		},
		{
			name:  "stamped with the version this change replaced",
			stamp: int64(ourStateVersion) - 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// The manager doing the reading predates the bump.
			supported := int64(ourStateVersion) - 1
			clients := storedStateClientsWithMigrations(&schemaListClient{
				pages: []*unstructured.UnstructuredList{{
					Items: []unstructured.Unstructured{
						migrationCarryingTheBumpedFields("tenant-a", "orders", test.stamp),
					},
				}},
			})
			err := VerifyStoredControllerState(context.Background(), clients, supported)
			if !test.wantRefusal {
				if err != nil {
					t.Fatalf("VerifyStoredControllerState error = %v, want the older contract to be admitted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("VerifyStoredControllerState admitted a manager that predates the stored state")
			}
			for _, want := range []string{
				"controller downgrade refused",
				"PtahMigration",
				"tenant-a/orders",
				"status.executionBinding.controllerStateVersion",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("VerifyStoredControllerState error = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

// Every location the scan covers has to refuse on its own. A fence that reads
// one path and reports on six is green over five it never looked at.
func TestTheStoredStateFenceRefusesAtEveryLocationItClaims(t *testing.T) {
	t.Parallel()
	for _, resourceKind := range storedControllerStateKinds {
		for _, location := range resourceKind.locations {
			t.Run(resourceKind.kind+"/"+location.name, func(t *testing.T) {
				t.Parallel()
				object := schemaWithControllerStateAt(
					"tenant-a", "resource-a", int64(newerStateVersion), location.path...)
				clients := emptyStoredStateClients()
				page := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{object}}
				switch resourceKind.kind {
				case "PtahSchema":
					clients.Schemas = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahSchemaPlan":
					clients.Plans = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahSchemaApproval":
					clients.Approvals = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahMigration":
					clients.Migrations = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahMigrationPlan":
					clients.MigrationPlans = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				case "PtahMigrationApproval":
					clients.MigrationApprovals = &schemaListClient{pages: []*unstructured.UnstructuredList{page}}
				default:
					t.Fatalf("no client is wired for %s, so this proof would pass over nothing", resourceKind.kind)
				}
				err := VerifyStoredControllerState(
					context.Background(), clients, int64(ourStateVersion))
				if err == nil {
					t.Fatalf("%s %s admitted state a newer manager wrote", resourceKind.kind, location.name)
				}
				for _, want := range []string{
					"controller downgrade refused",
					resourceKind.kind,
					location.name + ".controllerStateVersion",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal = %v, want it to name %q", err, want)
					}
				}
			})
		}
	}
}

// The release ratchet decides the other direction: which candidate may take
// over from the release that is active.
//
// Both rows matter. A ratchet that refused the bump would stop this change
// from ever being deployed, and one that admitted its reverse would let an
// operator roll back to a manager that cannot read what the release it
// replaces has already written.
func TestTheReleaseRatchetAdmitsTheBumpAndRefusesItsReverse(t *testing.T) {
	t.Parallel()
	const image = "registry.example/ptah-operator@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	tests := []struct {
		name        string
		candidate   int32
		active      int32
		wantRefusal string
	}{
		{
			name:      "the bump this change makes",
			candidate: ourStateVersion,
			active:    ourStateVersion - 1,
		},
		{
			name:        "its reverse",
			candidate:   ourStateVersion - 1,
			active:      ourStateVersion,
			wantRefusal: "release activation controller-state rollback refused",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			guard := &ReleaseActivationGuard{
				ReleaseSequence:          2,
				ControllerStateVersion:   test.candidate,
				AdmissionContractVersion: CurrentAdmissionContractVersion,
				ManagerImage:             image,
			}
			// The active release is the one before the candidate, so the
			// candidate is never judged against its own recorded identity.
			identity := releaseActivationIdentity{
				active:    1,
				release:   1,
				state:     uint64(test.active),
				admission: uint64(CurrentAdmissionContractVersion),
				image:     image,
				phase:     ControllerCredentialsActive,
			}
			err := guard.verifyCandidateCompatibility(identity)
			if test.wantRefusal == "" {
				if err != nil {
					t.Fatalf("verifyCandidateCompatibility error = %v, want the bump admitted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantRefusal) {
				t.Fatalf("verifyCandidateCompatibility error = %v, want %q", err, test.wantRefusal)
			}
		})
	}
}
