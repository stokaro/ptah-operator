package workload

import (
	"strings"
	"unicode/utf8"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	// EnvActor is how an Apply tells Ptah who it records: in
	// schema_migrations_log for a migration, and in ptah_schema_apply_log for
	// a schema. It fills `migrations up --actor` and `schema apply --actor`
	// through Ptah's own environment binding, so the name never reaches a
	// command line: it is not an argument the runner assembles, nothing can
	// read it as a flag, and no shell is involved.
	EnvActor = "PTAH_ACTOR"

	// EnvActorOrigin fills --actor-origin the same way. Ptah stores the word
	// in the actor_origin column beside the name, and refuses it without a
	// name, so a Job sets both or neither.
	EnvActorOrigin = "PTAH_ACTOR_ORIGIN"

	// ActorOriginAdmission says the name is the username admission stamped on
	// the approval the run consumed.
	ActorOriginAdmission = "admission"

	// ActorOriginPolicy says no person approved the run: apply: Always allowed
	// it, and the name is the fixed one under ReservedActorPrefix.
	ActorOriginPolicy = "policy"

	// MaxActorBytes bounds the name a run records, after encoding.
	//
	// Both of Ptah's actor columns are VARCHAR(256), and a value it refuses
	// costs more than the value: measured on Ptah v0.18.0, PostgreSQL 17 and
	// MySQL 8.4, the migration still applies and exits 0 and every one of its
	// log rows is lost, with only a warning on the executor's stderr. An
	// encoded name is ASCII, so its bytes and its characters are the same
	// number under every character set, and it holds no $, so the kubelet
	// cannot lengthen it on the way to the process.
	MaxActorBytes = 256

	// actorMarker ends a name that did not fit, so a reader can tell it
	// from a whole one.
	actorMarker = "..."

	// ReservedActorPrefix starts every name the operator records on its own
	// account. Admission refuses a migration approval from a username under it,
	// so the log cannot read a person's decision as a run nobody approved, or
	// the reverse. A schema approval is not refused, and the origin beside the
	// name is what tells the two apart there.
	ReservedActorPrefix = "system:ptah-operator:"

	// alwaysActorPrefix starts the name a run under apply: Always records. The
	// resource follows: a namespace and a name of 63 bytes each leave it well
	// inside the bound. The log it is written to says which kind it is.
	alwaysActorPrefix = ReservedActorPrefix + "apply-always:"

	upperHexDigits = "0123456789ABCDEF"
)

// MigrationActor is the name a migration Apply records in the database's own
// log: the username admission stamped on the approval it consumes, which is a
// field only the webhook writes, or, when no approval authorized the run
// because the policy is apply: Always, a fixed name that says so and names the
// resource. Either way it is encoded by encodedActor.
//
// The claim persists the result, so this runs once per claim. Reading the
// approval again at build time would make a deleted approval unbuildable, and
// a run the manager could no longer account for.
func MigrationActor(
	migration *operatorv1alpha1.PtahMigration,
	approval *operatorv1alpha1.PtahMigrationApproval,
) string {
	if approval != nil {
		return encodedActor(approval.Spec.Approver.Username)
	}
	return encodedActor(alwaysActorPrefix + migration.Namespace + "/" + migration.Name)
}

// MigrationActorOrigin is the word a migration Apply records beside its
// actor: admission when the claim names the approval it consumed, policy when
// it names none. The claim records both, so the Job a claim builds does not
// move with anything that happens to the approval afterwards.
func MigrationActorOrigin(operation operatorv1alpha1.MigrationOperationStatus) string {
	if operation.ApprovalRef != nil {
		return ActorOriginAdmission
	}
	return ActorOriginPolicy
}

// SchemaApplyActor is the name and the origin a schema Apply records in
// ptah_schema_apply_log, chosen as MigrationActor chooses: the username
// admission stamped on the approval the run consumed, which status.plan copies
// when the approval is consumed, or the fixed name under apply: Always when no
// approval was. An empty name means none is known, and the Job then sets
// neither variable.
//
// It reads status.plan at build time, unlike a migration, whose claim keeps its
// own copy. The builder refuses an Apply of anything but status.plan, and a
// plan that needs an approval without one, so every Job an Apply claim can
// build reads the same approval.
func SchemaApplyActor(schema *operatorv1alpha1.PtahSchema) (string, string) {
	if plan := schema.Status.Plan; plan != nil && plan.Approval != nil {
		name := encodedActor(plan.Approval.Approver.Username)
		if name == "" {
			return "", ""
		}
		return name, ActorOriginAdmission
	}
	return encodedActor(alwaysActorPrefix + schema.Namespace + "/" + schema.Name), ActorOriginPolicy
}

// encodedActor percent-encodes a name for the log: every byte outside
// 0x20-0x7E, and % and $, become %XX. Spaces, colons and the rest of printable
// ASCII stay, so an ordinary username or email reads unchanged, and
// `oidc:José García` reads `oidc:Jos%C3%A9 Garc%C3%ADa`.
//
// $ is there for the kubelet, which expands $(VAR) and reduces $$ in a
// container's environment values. An approver who controls their username claim
// could otherwise write `$(KUBERNETES_PORT)` a few times, pass the bound here,
// reach the process over it, and leave no log row with their name on it. With no
// $ in the value, what this bound measures is what the process sees.
//
// The manager cannot know the target database's character set, and it decides
// whether the run keeps a record at all: on Ptah v0.18.0 a MySQL database in
// latin1 or utf8mb3 refuses a name holding a character it cannot store, and
// every log row of that run is lost while the run reports success. ASCII is the
// one form every character set stores byte for byte. The encoding is lossless,
// because a person whose name is all outside ASCII would otherwise read as a row
// of placeholders, which answers nothing in an audit, and it also carries a
// control character safely: a NUL cannot be in a process's environment at all.
//
// A name over the bound is cut between two characters of the original, so no
// escape is cut in half and what remains decodes to valid text, and it ends in
// the marker.
func encodedActor(name string) string {
	name = strings.TrimSpace(name)
	var encoded strings.Builder
	fitsWithMarker := 0
	for rest := name; rest != ""; {
		_, width := utf8.DecodeRuneInString(rest)
		for _, value := range []byte(rest[:width]) {
			if value < 0x20 || value > 0x7E || value == '%' || value == '$' {
				encoded.WriteByte('%')
				encoded.WriteByte(upperHexDigits[value>>4])
				encoded.WriteByte(upperHexDigits[value&0x0F])
				continue
			}
			encoded.WriteByte(value)
		}
		rest = rest[width:]
		if encoded.Len() <= MaxActorBytes-len(actorMarker) {
			fitsWithMarker = encoded.Len()
		}
	}
	if encoded.Len() <= MaxActorBytes {
		return encoded.String()
	}
	return encoded.String()[:fitsWithMarker] + actorMarker
}
