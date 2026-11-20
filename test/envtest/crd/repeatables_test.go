package crd_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// What a migration's status says about its repeatable migrations is bounded
// the way the controller bounds it: a count that is not negative, at most 64
// unfinished and 64 orphaned identities, each once and no longer than a file
// name, and the identities a run applied held to the form a file name carries,
// since only a planned file can have applied. A record's identity comes from
// the database, so the two history lists take any text the runner cleaned.
func TestStatusBoundsTheRepeatableMigrations(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "repeatables")
	migration := migrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store the migration: %v", err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	identities := func(count int) []any {
		listed := make([]any, 0, count)
		for index := range count {
			listed = append(listed, fmt.Sprintf("view_%03d", index))
		}
		return listed
	}
	history := func(extra map[string]any) map[string]any {
		return merged(map[string]any{
			"observedAt": "2026-11-20T07:43:00Z", "contractVersion": int64(1), "currentVersion": int64(2),
			"appliedCount": int64(2), "pendingCount": int64(0),
			"fingerprint": digest, "targetIdentityDigest": digest,
		}, extra)
	}
	lastRun := func(applied []any) map[string]any {
		return map[string]any{"outcome": "Applied", "startedAt": "2026-11-20T07:41:53Z", "appliedRepeatables": applied}
	}

	for _, test := range []struct {
		name  string
		path  []string
		value map[string]any
	}{
		{"a pending count, 64 unfinished and 64 orphaned", []string{"status", "history"}, history(map[string]any{
			"pendingRepeatableCount": int64(3), "unfinishedRepeatables": identities(64), "orphanedRepeatables": identities(64),
		})},
		{"a record's identity the runner cleaned", []string{"status", "history"}, history(map[string]any{
			"unfinishedRepeatables": []any{"old [2J [redacted]"}, "orphanedRepeatables": []any{strings.Repeat("o", 255)},
		})},
		{"256 applied", []string{"status", "lastRun"}, lastRun(identities(256))},
	} {
		t.Run(test.name, func(t *testing.T) {
			admitted := reread(t, migration)
			set(t, admitted, test.value, test.path...)
			if err := api.Status().Update(context.Background(), admitted, client.DryRunAll); err != nil {
				t.Fatalf("the status refused %s: %v", test.name, err)
			}
			// A field the schema does not declare is pruned without a
			// complaint, so what came back says the fields reached the CRD.
			kept, _, err := unstructured.NestedMap(admitted.Object, test.path...)
			if err != nil || !reflect.DeepEqual(kept, test.value) {
				t.Fatalf("%s came back as %v, want %v: %v", strings.Join(test.path, "."), kept, test.value, err)
			}
		})
	}

	for _, test := range []struct {
		name  string
		path  []string
		value map[string]any
		want  cause
	}{
		{"a negative pending count", []string{"status", "history"}, history(map[string]any{"pendingRepeatableCount": int64(-1)}),
			cause{"status.history.pendingRepeatableCount", "should be greater than or equal to 0"}},
		{"65 unfinished", []string{"status", "history"}, history(map[string]any{"unfinishedRepeatables": identities(65)}),
			cause{"status.history.unfinishedRepeatables", "Too many"}},
		{"65 orphaned", []string{"status", "history"}, history(map[string]any{"orphanedRepeatables": identities(65)}),
			cause{"status.history.orphanedRepeatables", "Too many"}},
		{"one unfinished twice", []string{"status", "history"}, history(map[string]any{"unfinishedRepeatables": []any{"a", "a"}}),
			cause{"status.history.unfinishedRepeatables[1]", "Duplicate value"}},
		{"an orphaned identity no file name holds", []string{"status", "history"},
			history(map[string]any{"orphanedRepeatables": []any{strings.Repeat("o", 256)}}),
			cause{"status.history.orphanedRepeatables[0]", "Too long"}},
		{"257 applied", []string{"status", "lastRun"}, lastRun(identities(257)),
			cause{"status.lastRun.appliedRepeatables", "Too many"}},
		{"one applied twice", []string{"status", "lastRun"}, lastRun([]any{"a", "a"}),
			cause{"status.lastRun.appliedRepeatables[1]", "Duplicate value"}},
		{"an applied identity no file name carries", []string{"status", "lastRun"}, lastRun([]any{"item names"}),
			cause{"status.lastRun.appliedRepeatables[0]", "should match"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := reread(t, migration)
			set(t, refused, test.value, test.path...)
			err := api.Status().Update(context.Background(), refused, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Error(mismatch)
			}
		})
	}
}
