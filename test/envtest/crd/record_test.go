package crd_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The values a plan and the approvals that name it carry. They are the ones on
// the reference pages, so a row that departs from them departs from something a
// reader has seen.
const (
	artifactDigest           = "sha256:2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
	contentDigest            = "sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea"
	planFingerprint          = "sha256:71c480df93d6ae2f14efe3c44baabb7d3bc5d0e2de07d0e7a9b1a6cbd5f7ca3f"
	verificationPolicyUID    = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	verificationPolicyDigest = "sha256:084fed08b978af4d7d196a7446a86b58009e636b611db16211b65a9aadff29c5"
	desiredStateFingerprint  = "sha256:19581e27de7ced00ff1ce50b2047e7a567c76b1cbaebabe5ef03f7c3017bb5b7"
	actualStateFingerprint   = "sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5"
	coordinationDigest       = "sha256:e7f6c011776e8db7cd330b54174fd76f7d0216b612387a5ffcfb81e6f0919683"
	targetIdentityDigest     = "sha256:67586e98fad27da0b9968bc039a1ef34c939b9b8e523a8bef89d478608c5ecf6"
	policyFingerprint        = "sha256:fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9"
	historyFingerprint       = "sha256:4a44dc15364204a80fe80e9039455cc1608281820fe2b24f1e5233ade6af1dd5"
	ptahVersion              = "v0.8.1-54-gb689872e0"
	executorImage            = "ghcr.io/stokaro/ptah@sha256:1b4f0e9851971998e732078544c96b36c3d01cedf7caa332359d6f1d83567014"
	runnerImage              = "ghcr.io/stokaro/ptah-runner@sha256:60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"
	controllerImage          = "ghcr.io/stokaro/ptah-operator@sha256:fd61a03af4f77d870fc21e05e7e80678095c92d808cfb3b5c279ee04c74aca13"
	controllerRevision       = "a7d0119c0bd0d34e0b73f1d9e0e5c6aa0d9ff2b1"
	executionBindingID       = "v1-9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	schemaUID                = "4f2c9e1a-5b6d-4a7e-9c31-0d8f2b6a4e57"
	migrationUID             = "8d3f6c2b-1a4e-4f90-b7c5-2e6a8d0b3f41"
	planUID                  = "9a1b3c5d-7e9f-4012-83a4-5c6d7e8f9a0b"
	approvedAt               = "2026-09-20T09:14:02Z"
	mutationRequestUID       = "6f4b2a18-8c3e-4d5a-b1f7-2e0c9d8a7b64"
)

// executionBinding is what every plan and approval carries about the build
// that computed or will run it.
func executionBinding() map[string]any {
	return map[string]any{
		"executionBindingID":     executionBindingID,
		"ptahVersion":            ptahVersion,
		"executorImage":          executorImage,
		"runnerImage":            runnerImage,
		"runnerProtocolVersion":  int64(5),
		"controllerImage":        controllerImage,
		"controllerRevision":     controllerRevision,
		"controllerStateVersion": int64(2),
	}
}

func merged(parts ...map[string]any) map[string]any {
	result := map[string]any{}
	for _, part := range parts {
		for key, value := range part {
			result[key] = value
		}
	}
	return result
}

func schemaPlanSpec() map[string]any {
	return merged(executionBinding(), map[string]any{
		"schemaRef":      map[string]any{"name": "application", "uid": schemaUID},
		"fingerprint":    planFingerprint,
		"dialect":        "postgres",
		"destructive":    false,
		"statementCount": int64(4),
		"size":           int64(1832),
		"contentDigest":  contentDigest,
		"chunks": []any{map[string]any{
			"index": int64(0), "name": "ptah-plan-71c480df93d6ae2f14efe3c4-000", "key": "chunk",
			"size": int64(1832), "digest": contentDigest,
		}},
		"contractVersion":          int64(3),
		"artifactDigest":           artifactDigest,
		"verificationPolicyUID":    verificationPolicyUID,
		"verificationPolicyDigest": verificationPolicyDigest,
		"desiredStateFingerprint":  desiredStateFingerprint,
		"actualStateFingerprint":   actualStateFingerprint,
		"coordinationDigest":       coordinationDigest,
		"targetIdentityDigest":     targetIdentityDigest,
		"policyFingerprint":        policyFingerprint,
	})
}

func migrationPlanSpec() map[string]any {
	return merged(executionBinding(), map[string]any{
		"migrationRef":       map[string]any{"name": "orders", "uid": migrationUID},
		"fingerprint":        desiredStateFingerprint,
		"createdAt":          "2026-09-20T09:12:44Z",
		"currentVersion":     int64(12),
		"historyFingerprint": historyFingerprint,
		"migrations": []any{
			map[string]any{"version": int64(13), "versionKey": "0013", "description": "add order status index", "checksum": contentDigest, "transactionMode": "file"},
			map[string]any{"version": int64(14), "versionKey": "0014", "description": "backfill order status", "checksum": artifactDigest, "transactionMode": "file"},
		},
		"contractVersion":          int64(1),
		"artifactDigest":           artifactDigest,
		"verificationPolicyUID":    verificationPolicyUID,
		"verificationPolicyDigest": verificationPolicyDigest,
		"coordinationDigest":       coordinationDigest,
		"targetIdentityDigest":     targetIdentityDigest,
		"policyFingerprint":        policyFingerprint,
	})
}

// approvalStamp is what the admission webhook writes into an approval from the
// authenticated request. envtest runs no webhook, so the suite writes it.
func approvalStamp() map[string]any {
	return map[string]any{
		"approver":           map[string]any{"username": "jane@example.com", "uid": "1b9d6bcf-bbfd-4b2d-9b5d-ab8dfbbd4bed", "groups": []any{"schema-approvers"}},
		"approvedAt":         approvedAt,
		"mutationRequestUID": mutationRequestUID,
	}
}

func schemaApprovalSpec() map[string]any {
	return merged(executionBinding(), approvalStamp(), map[string]any{
		"schemaRef":                map[string]any{"name": "application", "uid": schemaUID},
		"planRef":                  map[string]any{"name": "ptah-plan-71c480df93d6ae2f14efe3c4", "uid": planUID},
		"planFingerprint":          planFingerprint,
		"artifactDigest":           artifactDigest,
		"verificationPolicyUID":    verificationPolicyUID,
		"verificationPolicyDigest": verificationPolicyDigest,
		"desiredStateFingerprint":  desiredStateFingerprint,
		"actualStateFingerprint":   actualStateFingerprint,
		"coordinationDigest":       coordinationDigest,
		"targetIdentityDigest":     targetIdentityDigest,
		"policyFingerprint":        policyFingerprint,
	})
}

func migrationApprovalSpec() map[string]any {
	return merged(executionBinding(), approvalStamp(), map[string]any{
		"migrationRef":             map[string]any{"name": "orders", "uid": migrationUID},
		"planRef":                  map[string]any{"name": "ptah-mplan-19581e27de7ced00ff1ce50b", "uid": planUID},
		"planFingerprint":          desiredStateFingerprint,
		"historyFingerprint":       historyFingerprint,
		"artifactDigest":           artifactDigest,
		"verificationPolicyUID":    verificationPolicyUID,
		"verificationPolicyDigest": verificationPolicyDigest,
		"coordinationDigest":       coordinationDigest,
		"targetIdentityDigest":     targetIdentityDigest,
		"policyFingerprint":        policyFingerprint,
	})
}

func realmSpec() map[string]any {
	return map[string]any{
		"engine":     "PostgreSQL",
		"namespaces": []any{"orders", "orders-next"},
		"sharing":    "Exclusive",
	}
}

func basedOn(kind, namespace, name string, spec func() map[string]any) func() *unstructured.Unstructured {
	return func() *unstructured.Unstructured { return resource(kind, namespace, name, spec()) }
}

// bindingRefusals hold for every plan and approval: the fields that identify
// the build are patterned, and a controller state version starts at one.
func bindingRefusals() []refusal {
	return []refusal{
		{
			name:   "controllerImage by tag rather than digest",
			mutate: setting("ghcr.io/stokaro/ptah-operator:0.1.0", "spec", "controllerImage"),
			want:   []cause{{"spec.controllerImage", "should match"}},
		},
		{
			name:   "controllerRevision with surrounding space",
			mutate: setting(" "+controllerRevision, "spec", "controllerRevision"),
			want:   []cause{{"spec.controllerRevision", "should match"}},
		},
		{
			name:   "controllerRevision past 128 bytes",
			mutate: setting(strings.Repeat("r", 129), "spec", "controllerRevision"),
			want:   []cause{{"spec.controllerRevision", "Too long"}},
		},
		{
			name:   "controllerStateVersion zero",
			mutate: setting(int64(0), "spec", "controllerStateVersion"),
			want:   []cause{{"spec.controllerStateVersion", "greater than or equal to 1"}},
		},
		{
			name:   "executionBindingID outside its pattern",
			mutate: setting("v2-9f8e7d6c5b4a39281706f5e4d3c2b1a0", "spec", "executionBindingID"),
			want:   []cause{{"spec.executionBindingID", "should match"}},
		},
		{
			name:   "executionBindingID is required",
			mutate: removing("spec", "executionBindingID"),
			want:   []cause{{"spec.executionBindingID", "Required value"}},
		},
		{
			name:   "controllerImage is required",
			mutate: removing("spec", "controllerImage"),
			want:   []cause{{"spec.controllerImage", "Required value"}},
		},
		{
			name:   "spec is required",
			mutate: removing("spec"),
			want:   []cause{{"spec", "Required value"}},
		},
	}
}

func TestPtahSchemaPlanRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "schema-plan-refusals")
	chunk := func(index int64) map[string]any {
		return map[string]any{"index": index, "name": "ptah-plan-71c480df93d6ae2f14efe3c4-000", "key": "chunk", "size": int64(1), "digest": contentDigest}
	}
	rows := append(bindingRefusals(),
		refusal{
			name:   "fingerprint is required",
			mutate: removing("spec", "fingerprint"),
			want:   []cause{{"spec.fingerprint", "Required value"}},
		},
		refusal{
			name:   "schemaRef.uid is required",
			mutate: removing("spec", "schemaRef", "uid"),
			want:   []cause{{"spec.schemaRef.uid", "Required value"}},
		},
		refusal{
			name:   "contractVersion outside its enum",
			mutate: setting(int64(2), "spec", "contractVersion"),
			want:   []cause{{"spec.contractVersion", "Unsupported value"}},
		},
		refusal{
			name:   "no chunks",
			mutate: setting([]any{}, "spec", "chunks"),
			want:   []cause{{"spec.chunks", "should have at least 1 items"}},
		},
		refusal{
			name:   "two chunks at one index",
			mutate: setting([]any{chunk(0), chunk(0)}, "spec", "chunks"),
			want:   []cause{{"spec.chunks[1]", "Duplicate value"}},
		},
		refusal{
			name:   "a chunk index past 15",
			mutate: setting([]any{chunk(16)}, "spec", "chunks"),
			want:   []cause{{"spec.chunks[0].index", "less than or equal to 15"}},
		},
		refusal{
			// Sixteen indexes exist, so seventeen chunks also repeat one; the
			// cause the row needs is the count.
			name: "seventeen chunks",
			mutate: func(t *testing.T, object *unstructured.Unstructured) {
				chunks := make([]any, 17)
				for index := range chunks {
					chunks[index] = chunk(int64(index % 16))
				}
				set(t, object, chunks, "spec", "chunks")
			},
			want: []cause{{"spec.chunks", "Too many"}},
		},
		refusal{
			name:   "a chunk past 512 KiB",
			mutate: setting([]any{merged(chunk(0), map[string]any{"size": int64(524289)})}, "spec", "chunks"),
			want:   []cause{{"spec.chunks[0].size", "less than or equal to 524288"}},
		},
		refusal{
			name:   "a chunk without a digest",
			mutate: setting([]any{map[string]any{"index": int64(0), "name": "ptah-plan-71c480df93d6ae2f14efe3c4-000", "key": "chunk", "size": int64(1)}}, "spec", "chunks"),
			want:   []cause{{"spec.chunks[0].digest", "Required value"}},
		},
		refusal{
			name:   "a plan past 8 MiB",
			mutate: setting(int64(8388609), "spec", "size"),
			want:   []cause{{"spec.size", "less than or equal to 8388608"}},
		},
		refusal{
			name:   "an empty plan",
			mutate: setting(int64(0), "spec", "size"),
			want:   []cause{{"spec.size", "greater than or equal to 1"}},
		},
		refusal{
			name:   "a privilege change outside its enum",
			mutate: setting([]any{"Superuser"}, "spec", "privilegeChanges"),
			want:   []cause{{"spec.privilegeChanges[0]", "Unsupported value"}},
		},
		refusal{
			name:   "a privilege change listed twice",
			mutate: setting([]any{"Grant", "Grant"}, "spec", "privilegeChanges"),
			want:   []cause{{"spec.privilegeChanges[1]", "Duplicate value"}},
		},
	)
	assertRefusals(t, basedOn("PtahSchemaPlan", namespace, "ptah-plan-71c480df93d6ae2f14efe3c4", schemaPlanSpec), rows)
}

func TestPtahMigrationPlanRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "migration-plan-refusals")
	step := func(version int64) map[string]any {
		return map[string]any{"version": version, "checksum": contentDigest}
	}
	rows := append(bindingRefusals(),
		refusal{
			name:   "historyFingerprint is required",
			mutate: removing("spec", "historyFingerprint"),
			want:   []cause{{"spec.historyFingerprint", "Required value"}},
		},
		refusal{
			name:   "migrationRef.name is required",
			mutate: removing("spec", "migrationRef", "name"),
			want:   []cause{{"spec.migrationRef.name", "Required value"}},
		},
		refusal{
			name:   "contractVersion past 1",
			mutate: setting(int64(2), "spec", "contractVersion"),
			want:   []cause{{"spec.contractVersion", "less than or equal to 1"}},
		},
		refusal{
			name:   "createdAt that is not a time",
			mutate: setting("yesterday", "spec", "createdAt"),
			want:   []cause{{"spec.createdAt", "date-time"}},
		},
		refusal{
			name:   "a negative currentVersion",
			mutate: setting(int64(-1), "spec", "currentVersion"),
			want:   []cause{{"spec.currentVersion", "greater than or equal to 0"}},
		},
		refusal{
			name:   "no migrations",
			mutate: setting([]any{}, "spec", "migrations"),
			want:   []cause{{"spec.migrations", "should have at least 1 items"}},
		},
		refusal{
			name: "257 migrations",
			mutate: func(t *testing.T, object *unstructured.Unstructured) {
				steps := make([]any, 257)
				for index := range steps {
					steps[index] = step(int64(index + 1))
				}
				set(t, object, steps, "spec", "migrations")
			},
			want: []cause{{"spec.migrations", "Too many"}},
		},
		refusal{
			name:   "a migration at version zero",
			mutate: setting([]any{step(0)}, "spec", "migrations"),
			want:   []cause{{"spec.migrations[0].version", "greater than or equal to 1"}},
		},
		refusal{
			name:   "a migration without a checksum",
			mutate: setting([]any{map[string]any{"version": int64(13)}}, "spec", "migrations"),
			want:   []cause{{"spec.migrations[0].checksum", "Required value"}},
		},
		refusal{
			name:   "a migration checksum past 128 bytes",
			mutate: setting([]any{merged(step(13), map[string]any{"checksum": strings.Repeat("c", 129)})}, "spec", "migrations"),
			want:   []cause{{"spec.migrations[0].checksum", "Too long"}},
		},
		refusal{
			name:   "a migration transactionMode all, which only a PtahSchema takes",
			mutate: setting([]any{merged(step(13), map[string]any{"transactionMode": "all"})}, "spec", "migrations"),
			want:   []cause{{"spec.migrations[0].transactionMode", "Unsupported value"}},
		},
		refusal{
			name:   "runnerProtocolVersion zero",
			mutate: setting(int64(0), "spec", "runnerProtocolVersion"),
			want:   []cause{{"spec.runnerProtocolVersion", "greater than or equal to 1"}},
		},
	)
	assertRefusals(t, basedOn("PtahMigrationPlan", namespace, "ptah-mplan-19581e27de7ced00ff1ce50b", migrationPlanSpec), rows)
}

// approvalRefusals hold for both approval kinds.
func approvalRefusals() []refusal {
	return []refusal{
		{
			name:   "approver is required",
			mutate: removing("spec", "approver"),
			want:   []cause{{"spec.approver", "Required value"}},
		},
		{
			name:   "approver.username is required",
			mutate: removing("spec", "approver", "username"),
			want:   []cause{{"spec.approver.username", "Required value"}},
		},
		{
			name:   "approver in 65 groups",
			mutate: setting(numbered("group-", 65), "spec", "approver", "groups"),
			want:   []cause{{"spec.approver.groups", "Too many"}},
		},
		{
			name:   "approvedAt that is not a time",
			mutate: setting("now", "spec", "approvedAt"),
			want:   []cause{{"spec.approvedAt", "date-time"}},
		},
		{
			name:   "mutationRequestUID is required",
			mutate: removing("spec", "mutationRequestUID"),
			want:   []cause{{"spec.mutationRequestUID", "Required value"}},
		},
		{
			name:   "planFingerprint is required",
			mutate: removing("spec", "planFingerprint"),
			want:   []cause{{"spec.planFingerprint", "Required value"}},
		},
		{
			name:   "planRef.uid is required",
			mutate: removing("spec", "planRef", "uid"),
			want:   []cause{{"spec.planRef.uid", "Required value"}},
		},
	}
}

func TestPtahSchemaApprovalRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "schema-approval-refusals")
	rows := append(append(bindingRefusals(), approvalRefusals()...),
		refusal{
			name:   "schemaRef is required",
			mutate: removing("spec", "schemaRef"),
			want:   []cause{{"spec.schemaRef", "Required value"}},
		},
		refusal{
			name:   "actualStateFingerprint is required",
			mutate: removing("spec", "actualStateFingerprint"),
			want:   []cause{{"spec.actualStateFingerprint", "Required value"}},
		},
	)
	assertRefusals(t, basedOn("PtahSchemaApproval", namespace, "approve-application-1", schemaApprovalSpec), rows)
}

func TestPtahMigrationApprovalRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "migration-approval-refusals")
	rows := append(append(bindingRefusals(), approvalRefusals()...),
		refusal{
			name:   "migrationRef is required",
			mutate: removing("spec", "migrationRef"),
			want:   []cause{{"spec.migrationRef", "Required value"}},
		},
		refusal{
			name:   "historyFingerprint is required",
			mutate: removing("spec", "historyFingerprint"),
			want:   []cause{{"spec.historyFingerprint", "Required value"}},
		},
		refusal{
			name:   "runnerProtocolVersion zero",
			mutate: setting(int64(0), "spec", "runnerProtocolVersion"),
			want:   []cause{{"spec.runnerProtocolVersion", "greater than or equal to 1"}},
		},
	)
	assertRefusals(t, basedOn("PtahMigrationApproval", namespace, "approve-orders-14", migrationApprovalSpec), rows)
}

func TestPtahRealmRefusals(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	rows := []refusal{
		{
			name:   "engine is required",
			mutate: removing("spec", "engine"),
			want:   []cause{{"spec.engine", "Required value"}},
		},
		{
			name:   "namespaces are required",
			mutate: removing("spec", "namespaces"),
			want:   []cause{{"spec.namespaces", "Required value"}},
		},
		{
			name:   "sharing is required",
			mutate: removing("spec", "sharing"),
			want:   []cause{{"spec.sharing", "Required value"}},
		},
		{
			name:   "sharing outside its enum",
			mutate: setting("Everyone", "spec", "sharing"),
			want:   []cause{{"spec.sharing", "Unsupported value"}},
		},
		{
			name:   "engine outside its pattern",
			mutate: setting("-postgres", "spec", "engine"),
			want:   []cause{{"spec.engine", "should match"}},
		},
		{
			name:   "no namespaces",
			mutate: setting([]any{}, "spec", "namespaces"),
			want:   []cause{{"spec.namespaces", "should have at least 1 items"}},
		},
		{
			name:   "a namespace listed twice",
			mutate: setting([]any{"orders", "orders"}, "spec", "namespaces"),
			want:   []cause{{"spec.namespaces[1]", "Duplicate value"}},
		},
		{
			name:   "a namespace outside its pattern",
			mutate: setting([]any{"Orders"}, "spec", "namespaces"),
			want:   []cause{{"spec.namespaces[0]", "should match"}},
		},
		{
			name:   "a namespace past 63 bytes",
			mutate: setting([]any{strings.Repeat("n", 64)}, "spec", "namespaces"),
			want:   []cause{{"spec.namespaces[0]", "Too long"}},
		},
		{
			name:   "257 namespaces",
			mutate: setting(numbered("team-", 257), "spec", "namespaces"),
			want:   []cause{{"spec.namespaces", "Too many"}},
		},
	}
	assertRefusals(t, basedOn("PtahRealm", "", "refusals-realm", realmSpec), rows)
}
