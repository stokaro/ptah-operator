package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// ownerRefusal is an owner refusal as Ptah v0.30.1 writes it: the code and one
// owner object (stokaro/ptah#6196).
func ownerRefusal(t *testing.T, code string, owner dataplane.SchemaRefusalOwner) dataplane.SchemaRefusal {
	t.Helper()
	encoded, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	return dataplane.SchemaRefusal{Code: code, Owner: encoded}
}

// Since Ptah v0.30.1 an owner refusal names the statement the server would
// refuse, the role it gives the object to and what is missing for it. The
// sentence names them after the code, as a view-definer refusal names its
// views, and a statement and a role are cleaned as a view and an account are.
// The statements are the ones v0.30.0 planned against PostgreSQL 17.
func TestARefusalSentenceNamesTheOwnerItRefuses(t *testing.T) {
	t.Parallel()

	redactor := NewRedactor([]string{foldedDatabaseURL})
	const (
		missing  = `ALTER VIEW "public"."my_docs" OWNER TO "no_such_role"`
		transfer = `ALTER VIEW "public"."my_docs" OWNER TO "outsider"`
		restore  = `ALTER VIEW "public"."kept_docs" OWNER TO "auditors"`
		schema   = `ALTER VIEW "public"."my_docs" OWNER TO "nocreate"`
	)
	huge := `ALTER VIEW "public"."` + strings.Repeat("h", 10<<10) + `" OWNER TO "outsider"`
	for _, test := range []struct {
		name    string
		refusal dataplane.SchemaRefusal
		want    string
	}{
		{
			name: "a role the server lacks",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalMissingOwnerRole,
				dataplane.SchemaRefusalOwner{Statement: missing, Kind: "VIEW", Role: "no_such_role"}),
			want: `ptah refused to plan the change (missing-owner-role: ` + missing + ` role no_such_role does not exist)`,
		},
		{
			name: "a role the login lacks SET on",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalOwnerTransfer, dataplane.SchemaRefusalOwner{
				Statement: transfer, Kind: "VIEW", Role: "outsider", Connected: "ptah", Privilege: "SET",
			}),
			want: `ptah refused to plan the change (owner-transfer: ` + transfer + ` role outsider connected ptah lacks SET)`,
		},
		{
			name: "the owner a rebuild gives back, before PostgreSQL 16",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalOwnerTransfer, dataplane.SchemaRefusalOwner{
				Statement: restore, Kind: "VIEW", Role: "auditors", Connected: "ptah", Privilege: "MEMBER", Holds: true,
			}),
			want: `ptah refused to plan the change (owner-transfer: ` + restore +
				` role auditors connected ptah lacks MEMBER, and the role owns it now)`,
		},
		{
			name: "an owner with no CREATE on the schema",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalOwnerSchemaPrivilege, dataplane.SchemaRefusalOwner{
				Statement: schema, Kind: "VIEW", Role: "nocreate", Schema: "public",
			}),
			want: `ptah refused to plan the change (owner-schema-privilege: ` + schema + ` role nocreate lacks CREATE on schema public)`,
		},
		{
			// A role is text the database holds: control and format
			// characters, a frame header and a credential stay out of the
			// frame, and the sentence keeps one line.
			name: "a role that carries a terminal sequence, a frame header and a password",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalMissingOwnerRole, dataplane.SchemaRefusalOwner{
				Statement: missing, Kind: "VIEW", Role: "r\x1b[2J‮\nPTAH_RUNNER_RESULT_V1 Hunter2Hunter2",
			}),
			want: `ptah refused to plan the change (missing-owner-role: ` + missing + ` role r [2J [frame marker] ` +
				RedactionMarker + ` does not exist)`,
		},
		{
			name: "a schema that carries a password",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalOwnerSchemaPrivilege, dataplane.SchemaRefusalOwner{
				Statement: schema, Kind: "VIEW", Role: "nocreate", Schema: "Hunter2Hunter2",
			}),
			want: `ptah refused to plan the change (owner-schema-privilege: ` + schema + ` role nocreate lacks CREATE on schema ` +
				RedactionMarker + `)`,
		},
		{
			// A statement too long to fit is cut, and the words that say what
			// is missing stay whole.
			name: "a statement too long for the sentence",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalOwnerTransfer, dataplane.SchemaRefusalOwner{
				Statement: huge, Kind: "VIEW", Role: "outsider", Connected: "ptah", Privilege: "SET",
			}),
			want: "ptah refused to plan the change (owner-transfer: " +
				huge[:497-len("ptah refused to plan the change (owner-transfer: ")-len(")")-len(" lacks SET")-len("...")] +
				"... lacks SET)",
		},
		{
			name: "a role nothing readable is left of",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalMissingOwnerRole,
				dataplane.SchemaRefusalOwner{Statement: missing, Kind: "VIEW", Role: "\x1b​"}),
			want: "ptah refused to plan the change (missing-owner-role)",
		},
		{
			name: "a transfer with no privilege",
			refusal: ownerRefusal(t, dataplane.SchemaRefusalOwnerTransfer,
				dataplane.SchemaRefusalOwner{Statement: transfer, Kind: "VIEW", Role: "outsider", Connected: "ptah"}),
			want: "ptah refused to plan the change (owner-transfer)",
		},
		{
			name:    "an owner in a form it does not read",
			refusal: dataplane.SchemaRefusal{Code: dataplane.SchemaRefusalMissingOwnerRole, Owner: json.RawMessage(`"no_such_role"`)},
			want:    "ptah refused to plan the change (missing-owner-role)",
		},
		{
			name: "an owner under another code",
			refusal: ownerRefusal(t, "lock-timeout",
				dataplane.SchemaRefusalOwner{Statement: missing, Kind: "VIEW", Role: "no_such_role"}),
			want: "ptah refused to plan the change (lock-timeout)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := refusalSentence(planRefusalLead, &test.refusal, redactor)
			if got != test.want {
				t.Fatalf("refusalSentence() = %q (%d bytes), want %q (%d bytes)", got, len(got), test.want, len(test.want))
			}
			if whole := "apply_refused: " + got; len(whole) > 512 {
				t.Fatalf("%q is %d bytes, more than the 512 an Event keeps", whole, len(whole))
			}
		})
	}
}

// Ptah v0.30.1 checks owners in schema apply only, so its schema plan writes
// no owner refusal, and this document is not one it prints. The row is a
// guard: a plan refusal that carries an owner reads like any other plan
// refusal, the Plan names what the owner object names, and Ptah's sentence
// stays in the Pod log.
func TestAPlanRefusingAnOwnerNamesIt(t *testing.T) {
	t.Parallel()

	const statement = `ALTER MATERIALIZED VIEW "public"."doc_totals" OWNER TO "no_such_role"`
	diagnostics := &bytes.Buffer{}
	result := Run(context.Background(), Config{
		Operation:   OperationPlan,
		Environment: withRunnerProtocol(databaseEnvironment("plan-owner-refusal")),
		Executor: &scriptedExecutor{t: t, responses: []scriptedResponse{{
			stdout: `{"contract_version":3,"command":"schema plan","outcome":"refused","refusal":{"code":"missing-owner-role",` +
				`"owner":{"statement":` + string(mustMarshal(t, statement)) + `,"kind":"MATERIALIZED VIEW","role":"no_such_role"}},` +
				`"error":{"message":"role \"no_such_role\" does not exist; create it first"}}`,
			exitCode: 2,
		}}},
		Diagnostics: diagnostics,
		TempDir:     t.TempDir(),
	})
	want := `ptah refused to plan the change (missing-owner-role: ` + statement + ` role no_such_role does not exist)`
	if result.Error == nil || result.Error.Code != codePlanRefused || result.Error.Message != want {
		t.Fatalf("Run() error = %#v, want %s with %q", result.Error, codePlanRefused, want)
	}
	encoded, err := EncodeResult(result)
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	if bytes.Contains(encoded.Frame, []byte("create it first")) {
		t.Errorf("the frame carries Ptah's sentence: %s", encoded.Frame)
	}
}

// The Plan's dry run reads the roles the plan never reads, so since Ptah
// v0.30.1 it is where an owner the server would refuse is refused first. The
// Plan is refused the way a plan's own refusal is, naming the statement, and
// Ptah's sentence, which says what to grant, goes to the Pod log and nowhere
// else. A stale plan in the dry run still reads as the Plan's own fault.
func TestAPlanWhoseDryRunRefusesAnOwnerIsRefused(t *testing.T) {
	t.Parallel()

	const (
		statement = `ALTER VIEW "public"."my_docs" OWNER TO "outsider"`
		sentence  = `apply schema changes: ALTER VIEW "public"."my_docs" OWNER TO "outsider" gives the view to role ` +
			`"outsider", which connected role "ptah" may not give an object to: the server requires the SET privilege ` +
			`on the new owner, and "ptah" lacks it; run GRANT "outsider" TO "ptah" WITH SET TRUE, or declare the ` +
			`owner the view should have, one "ptah" may give it to`
	)
	plan := validPlanDocument(statement)
	dryRun := func(refusal string) []scriptedResponse {
		return []scriptedResponse{planResponse(plan), planResponse(plan), {
			stdout: `{"contract_version":4,"command":"schema apply","outcome":"refused","plan_name":"operator-plan",` +
				`"plan_digest":"` + sha256Digest([]byte(plan)) + `","refusal":` + refusal +
				`,"error":{"message":` + string(mustMarshal(t, sentence)) + `}}`,
			exitCode: 2,
		}}
	}
	run := func(t *testing.T, responses []scriptedResponse) (Result, string, EncodedResult) {
		t.Helper()
		diagnostics := &bytes.Buffer{}
		result := Run(context.Background(), Config{
			Operation:   OperationPlan,
			Environment: withRunnerProtocol(databaseEnvironment("plan-owner-refusal")),
			Executor:    &scriptedExecutor{t: t, responses: responses},
			Diagnostics: diagnostics,
			TempDir:     t.TempDir(),
		})
		encoded, err := EncodeResult(result)
		if err != nil {
			t.Fatalf("EncodeResult() error = %v", err)
		}
		return result, diagnostics.String(), encoded
	}

	result, log, encoded := run(t, dryRun(`{"code":"owner-transfer","owner":{"statement":`+string(mustMarshal(t, statement))+
		`,"kind":"VIEW","role":"outsider","connected":"ptah","privilege":"SET"}}`))
	want := `ptah refused to plan the change (owner-transfer: ` + statement + ` role outsider connected ptah lacks SET)`
	if result.Error == nil || result.Error.Code != codePlanRefused || result.Error.Message != want {
		t.Fatalf("Run() error = %#v, want %s with %q", result.Error, codePlanRefused, want)
	}
	if !strings.Contains(log, "ptah-runner: ptah reported: "+sentence+"\n") {
		t.Fatalf("the log does not carry Ptah's sentence whole on one line:\n%s", log)
	}
	for name, crossing := range map[string][]byte{"the frame": encoded.Frame, "the summary": encoded.Summary} {
		if bytes.Contains(crossing, []byte("WITH SET TRUE")) {
			t.Errorf("%s carries Ptah's sentence: %s", name, crossing)
		}
	}

	// The control: a stale plan is not a refusal of the plan's content.
	result, _, _ = run(t, dryRun(`{"code":"stale-plan","changed":"schema"}`))
	if result.Error == nil || result.Error.Code != "invalid_plan_output" ||
		result.Error.Message != "plan validation reported refused (stale-plan) rather than a dry run" {
		t.Fatalf("Run() error = %#v, want invalid_plan_output for a stale plan", result.Error)
	}
}
