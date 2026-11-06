package workload

import (
	"strings"
	"unicode/utf8"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const (
	// EnvMigrationActor is how a migration Apply tells Ptah who it records in
	// schema_migrations_log. It fills `migrations up --actor` through Ptah's own
	// environment binding, so the name never reaches a command line: it is not
	// an argument the runner assembles, nothing can read it as a flag, and no
	// shell is involved.
	EnvMigrationActor = "PTAH_ACTOR"

	// MaxMigrationActorBytes bounds the name a run records, after encoding.
	//
	// Ptah's column is VARCHAR(256), and a value it refuses costs more than the
	// value: measured on Ptah v0.18.0, PostgreSQL 17 and MySQL 8.4, the run still
	// applies and exits 0 and every one of its log rows is lost, with only a
	// warning on the executor's stderr. An encoded name is ASCII, so its bytes
	// and its characters are the same number under every character set, and it
	// holds no $, so the kubelet cannot lengthen it on the way to the process.
	MaxMigrationActorBytes = 256

	// migrationActorMarker ends a name that did not fit, so a reader can tell it
	// from a whole one.
	migrationActorMarker = "..."

	// ReservedActorPrefix starts every name the operator records on its own
	// account. Admission refuses an approval from a username under it, so the log
	// cannot read a person's decision as a run nobody approved, or the reverse.
	ReservedActorPrefix = "system:ptah-operator:"

	// alwaysMigrationActorPrefix starts the name a run under apply: Always
	// records. The resource follows: a namespace and a name of 63 bytes each
	// leave it well inside the bound.
	alwaysMigrationActorPrefix = ReservedActorPrefix + "apply-always:"

	upperHexDigits = "0123456789ABCDEF"
)

// MigrationActor is the name a migration Apply records in the database's own
// log: the username admission stamped on the approval it consumes, which is a
// field only the webhook writes, or, when no approval authorized the run
// because the policy is apply: Always, a fixed name that says so and names the
// resource. Either way it is encoded by encodedMigrationActor.
//
// The claim persists the result, so this runs once per claim. Reading the
// approval again at build time would make a deleted approval unbuildable, and
// a run the manager could no longer account for.
func MigrationActor(
	migration *operatorv1alpha1.PtahMigration,
	approval *operatorv1alpha1.PtahMigrationApproval,
) string {
	if approval != nil {
		return encodedMigrationActor(approval.Spec.Approver.Username)
	}
	return encodedMigrationActor(alwaysMigrationActorPrefix + migration.Namespace + "/" + migration.Name)
}

// encodedMigrationActor percent-encodes a name for the log: every byte outside
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
func encodedMigrationActor(name string) string {
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
		if encoded.Len() <= MaxMigrationActorBytes-len(migrationActorMarker) {
			fitsWithMarker = encoded.Len()
		}
	}
	if encoded.Len() <= MaxMigrationActorBytes {
		return encoded.String()
	}
	return encoded.String()[:fitsWithMarker] + migrationActorMarker
}
