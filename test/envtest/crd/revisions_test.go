package crd_test

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// status.history.revisions is bounded the way the controller bounds it: at
// most 64 rows keyed by version, each a state Ptah reports for a row that
// exists, a duration that is not negative, and a description and a checksum
// no longer than a file name and a digest.
func TestStatusBoundsTheRevisionRows(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "revisions")
	migration := migrationBase(namespace)()
	if err := api.Create(context.Background(), migration); err != nil {
		t.Fatalf("store the migration: %v", err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	row := func(version int64) map[string]any {
		return map[string]any{
			"version": version, "description": "create accounts", "checksum": "h1:one",
			"state": "applied", "appliedAt": "2026-10-24T07:42:30Z", "executionMillis": int64(1840),
		}
	}
	rows := func(count int) []any {
		listed := make([]any, 0, count)
		for version := int64(count); version >= 1; version-- {
			listed = append(listed, row(version))
		}
		return listed
	}
	history := func(revisions []any) map[string]any {
		return map[string]any{
			"observedAt": "2026-10-24T07:43:00Z", "contractVersion": int64(1), "currentVersion": int64(64),
			"appliedCount": int64(64), "pendingCount": int64(0),
			"fingerprint": digest, "targetIdentityDigest": digest, "revisions": revisions,
		}
	}
	changed := func(change func(map[string]any)) []any {
		first := row(2)
		change(first)
		return []any{first, row(1)}
	}

	stored := reread(t, migration)
	set(t, stored, history(rows(64)), "status", "history")
	if err := api.Status().Update(context.Background(), stored, client.DryRunAll); err != nil {
		t.Fatalf("the status refused 64 revision rows: %v", err)
	}

	for _, test := range []struct {
		name      string
		revisions []any
		want      cause
	}{
		{"65 rows", rows(65), cause{"status.history.revisions", "Too many"}},
		{"the same version twice", []any{row(1), row(1)}, cause{"status.history.revisions[1]", "Duplicate value"}},
		{"a state with no row", changed(func(r map[string]any) { r["state"] = "pending" }),
			cause{"status.history.revisions[0].state", "Unsupported value"}},
		{"no version", changed(func(r map[string]any) { r["version"] = int64(0) }),
			cause{"status.history.revisions[0].version", "should be greater than or equal to 1"}},
		{"a negative duration", changed(func(r map[string]any) { r["executionMillis"] = int64(-1) }),
			cause{"status.history.revisions[0].executionMillis", "should be greater than or equal to 0"}},
		{"a description no file name holds", changed(func(r map[string]any) { r["description"] = strings.Repeat("d", 256) }),
			cause{"status.history.revisions[0].description", "Too long"}},
		{"a checksum no digest spells", changed(func(r map[string]any) { r["checksum"] = strings.Repeat("c", 129) }),
			cause{"status.history.revisions[0].checksum", "Too long"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			refused := reread(t, migration)
			set(t, refused, history(test.revisions), "status", "history")
			err := api.Status().Update(context.Background(), refused, client.DryRunAll)
			if mismatch := refusalMismatch(err, test.want); mismatch != nil {
				t.Error(mismatch)
			}
		})
	}
}
