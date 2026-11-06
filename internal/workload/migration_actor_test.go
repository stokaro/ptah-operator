package workload

import (
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func approvalStampedFor(username string) *operatorv1alpha1.PtahMigrationApproval {
	return &operatorv1alpha1.PtahMigrationApproval{
		ObjectMeta: metav1.ObjectMeta{
			Name: "approved-by-someone",
			// Anyone who can write the object writes these. Nothing reads them.
			Labels:      map[string]string{"approver": "forged"},
			Annotations: map[string]string{"approver": "forged"},
		},
		Spec: operatorv1alpha1.PtahMigrationApprovalSpec{
			Approver: operatorv1alpha1.ApprovalIdentity{
				Username: username, UID: "forged-uid", Groups: []string{"forged-group"},
			},
		},
	}
}

func TestMigrationActorIsTheUsernameAdmissionStamped(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	for _, row := range []struct {
		name     string
		username string
		want     string
	}{
		{"a plain name", "kubernetes-admin", "kubernetes-admin"},
		{"a service account", "system:serviceaccount:ptah:approver", "system:serviceaccount:ptah:approver"},
		{"an email", "denis@example.test", "denis@example.test"},
		{"spaces and colons", "oidc:Denis Voytyuk", "oidc:Denis Voytyuk"},
		{"shell and SQL metacharacters", `"; DROP TABLE t; -- 'q' \`, `"; DROP TABLE t; -- 'q' \`},
		{"a plus, which a form decoder would read as a space", "denis+ptah@example.test", "denis+ptah@example.test"},
		{"a leading dash", "-x --json", "-x --json"},
		{"what the column holds exactly", strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{"surrounding space, which Ptah trims too", "  someone  ", "someone"},
		{"an accent", "oidc:José García", "oidc:Jos%C3%A9 Garc%C3%ADa"},
		{"Cyrillic and an emoji", "oidc:Дмитрий 😀", "oidc:%D0%94%D0%BC%D0%B8%D1%82%D1%80%D0%B8%D0%B9 %F0%9F%98%80"},
		{"a name with no ASCII at all", "田中", "%E7%94%B0%E4%B8%AD"},
		{"a percent sign, which would otherwise be ambiguous", "50%off", "50%25off"},
		// The kubelet expands $(VAR) in a container's environment and reduces $$
		// to $, so a $ reaching the Job would not reach the process as written.
		{"a variable reference the kubelet would expand", "oidc:$(HOME)", "oidc:%24(HOME)"},
		{"a doubled dollar the kubelet would reduce", "a$$b", "a%24%24b"},
		{"a lone dollar", "a$b", "a%24b"},
		{"text that already reads as an escape", "already%20encoded", "already%2520encoded"},
		{"a NUL, which no process environment can hold", "some\x00one", "some%00one"},
		{"a line break and an escape sequence", "some\none\x1b[0m", "some%0Aone%1B[0m"},
		{"the last and first bytes outside the printable range", "\x1f \x7f", "%1F %7F"},
		{"bytes that are not UTF-8", "some\xffone", "some%FFone"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := MigrationActor(migration, approvalStampedFor(row.username)); got != row.want {
				t.Fatalf("MigrationActor(%q) = %q, want %q", row.username, got, row.want)
			}
		})
	}
}

// Every value an authenticator could produce comes out as printable ASCII, and
// decodes back to what it was. A decoder that reads + as a space would not, and
// the documentation names one that does not.
func TestMigrationActorIsPrintableASCIIAndDecodesBack(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	for _, username := range []string{
		"kubernetes-admin",
		"denis+ptah@example.test",
		"oidc:José García",
		"oidc:Дмитрий 😀",
		"田中",
		"50%off",
		"already%20encoded",
		"%",
		"$(HOME)",
		"$$(HOME)",
		"$",
		"some\x00one",
		"some\none\x1b[0m",
		"some\xffone",
		"\x7f\x80",
	} {
		t.Run(username, func(t *testing.T) {
			t.Parallel()
			got := MigrationActor(migration, approvalStampedFor(username))
			assertPrintableASCII(t, got)
			if strings.Contains(got, "$") {
				t.Fatalf("%q still holds a $, which the kubelet would expand", got)
			}
			decoded, err := url.PathUnescape(got)
			if err != nil {
				t.Fatalf("%q does not decode: %v", got, err)
			}
			if decoded != username {
				t.Fatalf("%q decodes to %q, want %q", got, decoded, username)
			}
		})
	}
}

func assertPrintableASCII(t *testing.T, value string) {
	t.Helper()
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7E {
			t.Fatalf("byte %d of %q is outside printable ASCII", index, value)
		}
	}
}

// Ptah loses every log row of a run whose name the column refuses, so a name
// whose encoded form is over the bound has to arrive shortened and say that it
// was. The bound is the encoded length, and a name can fit before encoding and
// not after it.
func TestMigrationActorShortensANameTheColumnWouldRefuse(t *testing.T) {
	t.Parallel()

	a := func(count int) string { return strings.Repeat("a", count) }
	migration := migrationFixture()
	for _, row := range []struct {
		name     string
		username string
		want     string
	}{
		// Literal numbers: a row derived from the constant follows it when it moves.
		{"one byte over", a(257), a(253) + "..."},
		{"far over", a(4096), a(253) + "..."},
		// 18 bytes as written, 20 encoded, and 266 bytes at the process if the
		// kubelet were left to expand them, past the column with no row kept.
		{"variable references that would pass the bound and then expand past it", strings.Repeat("$(KUBERNETES_PORT)", 14),
			strings.Repeat("%24(KUBERNETES_PORT)", 12) + "%24(KUBERNETE..."},
		{"few characters that expand past the bound", strings.Repeat("Д", 100), strings.Repeat("%D0%94", 42) + "..."},
		{"an emoji of twelve encoded bytes", strings.Repeat("😀", 100), strings.Repeat("%F0%9F%98%80", 21) + "..."},
		{"percent signs, which expand threefold", strings.Repeat("%", 100), strings.Repeat("%25", 84) + "..."},
		// The room for the name is 253 bytes. Each row below puts an escape at
		// that edge.
		{"an escape that ends exactly where the room ends", a(247) + "é" + "bbbb", a(247) + "%C3%A9" + "..."},
		{"an escape that would be cut after its percent sign", a(252) + "é" + "b", a(252) + "..."},
		{"an escape that would be cut after its first digit", a(251) + "é" + "bb", a(251) + "..."},
		{"a character cut between its two escapes", a(250) + "é" + "bbbb", a(250) + "..."},
		{"a character of four escapes cut after its first", a(244) + "😀" + "bbbb", a(244) + "..."},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := MigrationActor(migration, approvalStampedFor(row.username))
			if got != row.want {
				t.Fatalf("MigrationActor() = %q, want %q", got, row.want)
			}
			if len(got) > 256 {
				t.Fatalf("the name is %d bytes, over the 256 the column holds", len(got))
			}
			assertPrintableASCII(t, got)
			kept, err := url.PathUnescape(strings.TrimSuffix(got, "..."))
			if err != nil {
				t.Fatalf("the cut broke an escape: %q: %v", got, err)
			}
			if !utf8.ValidString(kept) {
				t.Fatalf("the cut broke a character: %q decodes to %q", got, kept)
			}
		})
	}
}

// A name whose encoded form is exactly the bound is whole: it keeps what it
// ends in, an escape included, and takes no marker.
func TestMigrationActorKeepsANameThatFitsExactly(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	for _, row := range []struct {
		name     string
		username string
		want     string
	}{
		{"ending in an escape", strings.Repeat("a", 250) + "é", strings.Repeat("a", 250) + "%C3%A9"},
		{"ending in printable ASCII", "é" + strings.Repeat("a", 250), "%C3%A9" + strings.Repeat("a", 250)},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if len(row.want) != 256 {
				t.Fatalf("the fixture is %d bytes, so this row is not about the bound", len(row.want))
			}
			if got := MigrationActor(migration, approvalStampedFor(row.username)); got != row.want {
				t.Fatalf("MigrationActor() = %q, want %q", got, row.want)
			}
		})
	}
}

func TestMigrationActorNamesThePolicyAndTheResourceWhenNoPersonApproved(t *testing.T) {
	t.Parallel()

	migration := migrationFixture()
	migration.Namespace, migration.Name = "billing", "orders"
	got := MigrationActor(migration, nil)
	if want := "system:ptah-operator:apply-always:billing/orders"; got != want {
		t.Fatalf("MigrationActor() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, ReservedActorPrefix) {
		t.Fatalf("%q is not under the prefix admission refuses to an approver, %q", got, ReservedActorPrefix)
	}

	// The longest names the API server stores still fit whole.
	migration.Namespace, migration.Name = strings.Repeat("n", 63), strings.Repeat("m", 63)
	got = MigrationActor(migration, nil)
	if strings.HasSuffix(got, "...") || !strings.HasSuffix(got, migration.Namespace+"/"+migration.Name) {
		t.Fatalf("the longest resource name did not fit whole: %q", got)
	}
}

// The name reaches the process that writes the log and no other, and it reaches
// it through the environment: no argument, so nothing assembles it into a
// command line, and a literal, so what the Pod runs is what the claim says.
func TestBuildMigrationApplyCarriesTheActorToTheExecutorAlone(t *testing.T) {
	t.Parallel()

	const actor = "oidc:Jos%C3%A9 Garc%C3%ADa"
	operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
	operation.Actor = actor
	job, err := builderFixture().BuildMigration(migrationFixture(), operation, migrationPlanFixture())
	if err != nil {
		t.Fatalf("BuildMigration() error = %v", err)
	}

	carried := requireEnv(t, job, EnvMigrationActor)
	if carried.Value != actor || carried.ValueFrom != nil {
		t.Fatalf("the executor's actor = %#v, want the literal %q", carried, actor)
	}
	template := job.Spec.Template
	if len(template.Spec.InitContainers) == 0 {
		t.Fatal("the Apply Job has no init containers, so the row below examines only the executor")
	}
	for index, container := range slices.Concat(template.Spec.InitContainers, template.Spec.Containers) {
		if index >= len(template.Spec.InitContainers) && container.Name == template.Spec.Containers[0].Name {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name == EnvMigrationActor || strings.Contains(variable.Value, actor) {
				t.Errorf("container %s carries the actor in %s", container.Name, variable.Name)
			}
		}
	}
	for _, container := range slices.Concat(template.Spec.InitContainers, template.Spec.Containers) {
		if slices.ContainsFunc(slices.Concat(container.Command, container.Args), func(argument string) bool {
			return strings.Contains(argument, actor)
		}) {
			t.Errorf("container %s carries the actor on its command line", container.Name)
		}
	}
	if strings.Contains(strings.Join(mapValues(job.Annotations), "\n"), actor) ||
		strings.Contains(strings.Join(mapValues(template.Annotations), "\n"), actor) {
		t.Error("the Job's annotations carry the actor")
	}
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

// Only an Apply writes the log. A read-only operation has no use for a person's
// name, whatever the claim says.
func TestBuildMigrationGivesAReadingNoActor(t *testing.T) {
	t.Parallel()

	operation := migrationOperationFixture(operatorv1alpha1.MigrationOperationHistory)
	operation.Actor = "someone"
	job, err := builderFixture().BuildMigration(migrationFixture(), operation, nil)
	if err != nil {
		t.Fatalf("BuildMigration() error = %v", err)
	}
	if _, found := envMap(job)[EnvMigrationActor]; found {
		t.Fatal("a History Job carries an actor")
	}
}

// A claim written by a release without the field carries no name. Its Job must
// build as that release built it, or an upgrade under a running Apply records
// the run as outcome unknown: the name adds one variable and moves nothing else
// a claim, a plan or an approval is bound to.
func TestBuildMigrationApplyWithoutAnActorSetsNoVariable(t *testing.T) {
	t.Parallel()

	builder, migration, plan := builderFixture(), migrationFixture(), migrationPlanFixture()
	claim := migrationOperationFixture(operatorv1alpha1.MigrationOperationApply)
	predecessor, err := builder.BuildMigration(migration, claim, plan)
	if err != nil {
		t.Fatalf("BuildMigration() error = %v", err)
	}
	if _, found := envMap(predecessor)[EnvMigrationActor]; found {
		t.Fatal("a claim without a name built a Job with one")
	}

	claim.Actor = "someone"
	named, err := builder.BuildMigration(migration, claim, plan)
	if err != nil {
		t.Fatalf("BuildMigration() error = %v", err)
	}
	if named.Name != predecessor.Name {
		t.Fatalf("the name moved the Job's identity: %q, want %q", named.Name, predecessor.Name)
	}
	stripped := withoutExecutorVariable(named, EnvMigrationActor)
	if reflect.DeepEqual(stripped, named) {
		t.Fatal("the Job carries no actor to remove, so the comparison below proves nothing")
	}
	if !reflect.DeepEqual(stripped, predecessor) {
		t.Fatalf("the name changed more than one variable:\n%#v\nwant\n%#v", stripped.Spec.Template, predecessor.Spec.Template)
	}
}

func withoutExecutorVariable(job *batchv1.Job, name string) *batchv1.Job {
	stripped := job.DeepCopy()
	executor := &stripped.Spec.Template.Spec.Containers[0]
	executor.Env = slices.DeleteFunc(executor.Env, func(variable corev1.EnvVar) bool { return variable.Name == name })
	return stripped
}
