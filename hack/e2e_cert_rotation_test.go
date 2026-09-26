// Copyright 2026 The Ptah Operator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCertificateE2EGeneratedSecretRequiresExactSource(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"valid", "name", "namespace", "type", "labels", "annotations", "tls.key", "ca.key", "extra data"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			secret := certificateSecretFixture()
			metadata := secret["metadata"].(map[string]any)
			switch field {
			case "name", "namespace":
				metadata[field] = "foreign"
			case "type":
				secret["type"] = "Opaque"
			case "labels", "annotations":
				metadata[field].(map[string]any)["unexpected"] = "foreign"
			case "tls.key":
				secret["data"].(map[string]any)[field] = ""
			case "ca.key":
				delete(secret["data"].(map[string]any), field)
			case "extra data":
				secret["data"].(map[string]any)["unexpected"] = "foreign"
			}
			fixture.writeJSON("secret-before.json", secret)
			output, err := fixture.run("validate_generated_secret \"$SECRET_BEFORE\"\n")
			if (err == nil) != (field == "valid") {
				t.Fatalf("source validation error = %v, output = %s", err, output)
			}
		})
	}
}

func TestCertificateE2EManagedWebhookInventory(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"mutatingwebhookconfiguration", "validatingwebhookconfiguration"} {
		for _, scenario := range []string{
			"uniform", "no dormant canary", "foreign namespace", "foreign path", "extra entry on the Service",
			"missing entry", "divergent bundle", "empty bundle",
		} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				fixture := newCertificateShellFixture(t)
				entries := [][3]string{
					{"mapproval.operator.ptah.run", "webhook", "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"},
					{"mmigrationapproval.operator.ptah.run", "webhook", "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval"},
				}
				canaryName := "certificate-rotation-canary-mutate.operator.ptah.run"
				if kind == "validatingwebhookconfiguration" {
					entries = [][3]string{
						{"vapproval.operator.ptah.run", "webhook", "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"},
						{"vmigrationapproval.operator.ptah.run", "webhook", "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval"},
						{"vpodintent.operator.ptah.run", "webhook", "/validate-v1-pod-ptah-operation-intent"},
						{"vcontrollerwrite.operator.ptah.run", "webhook", "/validate-operator-controller-write"},
					}
					canaryName = "certificate-rotation-canary-validate.operator.ptah.run"
				}
				webhooks := make([]map[string]any, 0, len(entries)+1)
				for _, entry := range entries {
					webhooks = append(webhooks, map[string]any{"name": entry[0], "clientConfig": map[string]any{
						"caBundle": "current-ca", "service": map[string]any{
							"name": entry[1], "namespace": "operator", "port": 443, "path": entry[2],
						},
					}})
				}
				// The dormant canary keeps whatever bundle it last had; the
				// rotator no longer maintains it, and the check must not either.
				webhooks = append(webhooks, map[string]any{"name": canaryName, "clientConfig": map[string]any{
					"caBundle": "stale-ca", "service": map[string]any{
						"name": "candidate", "namespace": "operator", "port": 443, "path": "/candidate/mutate",
					},
				}})
				first := webhooks[0]["clientConfig"].(map[string]any)
				switch scenario {
				case "no dormant canary":
					webhooks = webhooks[:len(webhooks)-1]
				case "foreign namespace":
					first["service"].(map[string]any)["namespace"] = "foreign"
				case "foreign path":
					first["service"].(map[string]any)["path"] = "/foreign"
				case "extra entry on the Service":
					webhooks = append(webhooks, map[string]any{"name": "extra.operator.ptah.run", "clientConfig": map[string]any{
						"caBundle": "current-ca", "service": map[string]any{
							"name": "webhook", "namespace": "operator", "port": 443, "path": "/extra",
						},
					}})
				case "missing entry":
					webhooks = webhooks[1:]
				case "divergent bundle":
					first["caBundle"] = "other-ca"
				case "empty bundle":
					first["caBundle"] = ""
				}
				fixture.writeJSON("api-secret.json", map[string]any{"webhooks": webhooks})
				output, err := fixture.run("uniform_service_bundle " + kind + " admission\n")
				if err != nil {
					t.Fatalf("bundle inspection: %v\n%s", err, output)
				}
				wantUniform := scenario == "uniform" || scenario == "no dormant canary"
				if (strings.TrimSpace(output) == "current-ca") != wantUniform {
					t.Fatalf("bundle result %q, want uniform %v", output, wantUniform)
				}
			})
		}
	}
}

func TestCertificateE2EExpandedTransitionTime(t *testing.T) {
	t.Parallel()
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	record := func() map[string]any {
		return map[string]any{
			"kind":     "Secret",
			"metadata": map[string]any{"name": "cert-stage", "namespace": "operator"},
			"data": map[string]any{
				"format":           encode("v3"),
				"phase":            encode("expanded"),
				"expanded-at":      encode("2026-09-26T12:00:00Z"),
				"candidate.ca.key": encode("CA-PRIVATE-KEY-FIXTURE"),
			},
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "expanded record"},
		{name: "prepared record", mutate: func(secret map[string]any) {
			secret["data"].(map[string]any)["phase"] = encode("prepared")
		}},
		{name: "one-pass record", mutate: func(secret map[string]any) {
			secret["data"].(map[string]any)["format"] = encode("v2")
		}},
		{name: "fractional time", mutate: func(secret map[string]any) {
			secret["data"].(map[string]any)["expanded-at"] = encode("2026-09-26T12:00:00.5Z")
		}},
		{name: "offset time", mutate: func(secret map[string]any) {
			secret["data"].(map[string]any)["expanded-at"] = encode("2026-09-26T14:00:00+02:00")
		}},
		{name: "no time", mutate: func(secret map[string]any) {
			delete(secret["data"].(map[string]any), "expanded-at")
		}},
		{name: "cleared record", mutate: func(secret map[string]any) { delete(secret, "data") }},
		{name: "another Secret", mutate: func(secret map[string]any) {
			secret["metadata"].(map[string]any)["name"] = "foreign"
		}},
		{name: "another namespace", mutate: func(secret map[string]any) {
			secret["metadata"].(map[string]any)["namespace"] = "foreign"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			secret := record()
			if test.mutate != nil {
				test.mutate(secret)
			}
			fixture.writeJSON("staging.json", secret)
			output, err := fixture.run("expanded_transition_time \"$UPGRADE_WORK_DIR/staging.json\"\n")
			accepted := err == nil
			if accepted != (test.mutate == nil) {
				t.Fatalf("accepted = %v, output %q", accepted, output)
			}
			if accepted && strings.TrimSpace(output) != "2026-09-26T12:00:00Z" {
				t.Fatalf("expansion time = %q", output)
			}
			if strings.Contains(output, "PRIVATE-KEY") {
				t.Fatal("the filter printed staged key material")
			}
		})
	}
}

func TestCertificateE2ERotatorCertificateWriteTime(t *testing.T) {
	t.Parallel()
	entry := func(manager, operation string, fields map[string]any) map[string]any {
		return map[string]any{
			"manager": manager, "operation": operation, "time": "2026-09-26T12:01:00Z",
			"fieldsV1": map[string]any{"f:data": fields},
		}
	}
	certificateFields := map[string]any{"f:ca.crt": map[string]any{}, "f:tls.crt": map[string]any{}}
	for _, test := range []struct {
		name    string
		entries []any
		want    bool
	}{
		{name: "rotator wrote the certificate", want: true, entries: []any{
			entry("helm", "Update", map[string]any{"f:ca.key": map[string]any{}}),
			entry("ptah-cert-rotator", "Update", certificateFields),
		}},
		{name: "another manager wrote it", entries: []any{entry("kubectl-patch", "Update", certificateFields)}},
		{name: "server-side apply by the rotator's name", entries: []any{entry("ptah-cert-rotator", "Apply", certificateFields)}},
		{name: "rotator owns other fields only", entries: []any{
			entry("ptah-cert-rotator", "Update", map[string]any{"f:ca.crt": map[string]any{}}),
		}},
		{name: "a subresource write", entries: []any{func() map[string]any {
			status := entry("ptah-cert-rotator", "Update", certificateFields)
			status["subresource"] = "status"
			return status
		}()}},
		{name: "two rotator entries", entries: []any{
			entry("ptah-cert-rotator", "Update", certificateFields),
			entry("ptah-cert-rotator", "Update", certificateFields),
		}},
		{name: "no field management", entries: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			fixture.writeJSON("primary.json", map[string]any{"metadata": map[string]any{"managedFields": test.entries}})
			output, err := fixture.run("rotator_certificate_write_time \"$UPGRADE_WORK_DIR/primary.json\"\n")
			if (err == nil) != test.want {
				t.Fatalf("accepted = %v, output %q", err == nil, output)
			}
			if test.want && strings.TrimSpace(output) != "2026-09-26T12:01:00Z" {
				t.Fatalf("write time = %q", output)
			}
		})
	}
}

func TestCertificateE2ESwitchedAfterDelay(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		switched string
		want     bool
	}{
		{name: "exactly at the switch time", switched: "2026-09-26T12:01:00Z", want: true},
		{name: "later", switched: "2026-09-26T13:00:00Z", want: true},
		{name: "one second early", switched: "2026-09-26T12:00:59Z"},
		{name: "before the expansion", switched: "2026-09-26T11:00:00Z"},
		{name: "not a time", switched: "soon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			output, err := fixture.run("switched_after_delay " + test.switched + " 2026-09-26T12:00:00Z 60\n")
			if (err == nil) != test.want {
				t.Fatalf("accepted = %v, output %q", err == nil, output)
			}
		})
	}
}

func TestCertificateE2ECASwitchDelaySeconds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "seconds", args: []string{"--run-interval=168h", "--ca-switch-delay=60s"}, want: "60"},
		{name: "hours", args: []string{"--ca-switch-delay=6h"}},
		{name: "zero", args: []string{"--ca-switch-delay=0s"}},
		{name: "compound", args: []string{"--ca-switch-delay=1m30s"}},
		{name: "no unit", args: []string{"--ca-switch-delay=60"}},
		{name: "absent", args: []string{"--run-interval=168h"}},
		{name: "twice", args: []string{"--ca-switch-delay=60s", "--ca-switch-delay=60s"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			fixture.writeJSON("rotator.json", map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "certificate-rotator", "args": test.args}},
			}}}})
			output, err := fixture.run("ROTATOR_DEPLOYMENT_JSON=$(cat \"$UPGRADE_WORK_DIR/rotator.json\")\nca_switch_delay_seconds\n")
			if (err == nil) != (test.want != "") {
				t.Fatalf("accepted = %v, output %q", err == nil, output)
			}
			if test.want != "" && strings.TrimSpace(output) != test.want {
				t.Fatalf("delay seconds = %q, want %q", output, test.want)
			}
			if test.want == "" && !strings.Contains(output, "--ca-switch-delay") {
				t.Fatalf("refusal does not name the flag: %q", output)
			}
		})
	}
}

func TestCertificateE2EPrimarySecretState(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		present bool
		error   string
		want    string
		wantErr bool
	}{
		{name: "present", present: true, want: "CA-FIXTURE original"},
		{name: "absent", error: `Error from server (NotFound): secrets "webhook-cert" not found`, want: "absent"},
		{name: "forbidden", error: `Error from server (Forbidden): secrets "webhook-cert" is forbidden`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			if test.present {
				fixture.writeJSON("api-secret.json", certificateSecretFixture())
			} else {
				fixture.write("api-error.txt", test.error)
			}
			output, err := fixture.run("state=$(primary_secret_state)\nprintf '%s\\n' \"$state\"\n")
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, output %q", err, output)
			}
			if !test.wantErr && strings.TrimSpace(output) != test.want {
				t.Fatalf("state = %q, want %q", output, test.want)
			}
		})
	}
}

type certificateShellFixture struct {
	t         *testing.T
	directory string
}

func newCertificateShellFixture(t *testing.T) *certificateShellFixture {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("certificate E2E behavioral tests require jq")
	}
	directory, err := os.MkdirTemp(t.TempDir(), "ptah-operator-cert-upgrade.")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &certificateShellFixture{t: t, directory: directory}
	source, err := os.ReadFile("e2e-cert-rotation.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	extract := func(start, end string) string {
		first, last := strings.Index(text, start), strings.Index(text, end)
		if first < 0 || last <= first {
			t.Fatalf("cannot extract certificate E2E functions between %q and %q", start, end)
		}
		return text[first:last]
	}
	fixture.write("functions.sh", extract("uniform_service_bundle()", "generate_upgrade_ca()")+
		extract("validate_generated_secret()", "trap cleanup_upgrade_files EXIT"))
	fixture.write("kubectl", `#!/bin/sh
set -eu
for argument in "$@"; do
  if [ "$argument" = get ]; then
    if [ -f "$UPGRADE_WORK_DIR/api-error.txt" ]; then
      cat "$UPGRADE_WORK_DIR/api-error.txt" >&2
      exit 1
    fi
    exec cat "$UPGRADE_WORK_DIR/api-secret.json"
  fi
done
exit 2
`)
	if err := os.Chmod(filepath.Join(directory, "kubectl"), 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *certificateShellFixture) write(name, contents string) {
	fixture.t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.directory, name), []byte(contents), 0o600); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *certificateShellFixture) writeJSON(name string, value any) {
	fixture.t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.write(name, string(contents))
}

func (fixture *certificateShellFixture) run(body string) (string, error) {
	fixture.t.Helper()
	fixture.write("run.sh", `set -eu
umask 077
SECRET_NAME=webhook-cert
OPERATOR_NAMESPACE=operator
HELM_RELEASE=ptah
KUBECONFIG_FILE=unused
SERVICE=webhook
STAGING_SECRET_NAME=cert-stage
SECRET_BEFORE=$UPGRADE_WORK_DIR/secret-before.json
SECRET_ERROR=$UPGRADE_WORK_DIR/secret-error.log
PRIMARY_OBSERVATION=$UPGRADE_WORK_DIR/primary-observation.json
PRIMARY_OBSERVATION_ERROR=$UPGRADE_WORK_DIR/primary-observation.err
fail() {
	printf '%s\n' "$*" >&2
	exit 1
}
. "$UPGRADE_WORK_DIR/functions.sh"
`+body)
	command := exec.Command("sh", filepath.Join(fixture.directory, "run.sh"))
	command.Env = append(os.Environ(), "UPGRADE_WORK_DIR="+fixture.directory,
		"PATH="+fixture.directory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+filepath.Dir(fixture.directory))
	output, err := command.CombinedOutput()
	return string(output), err
}

func certificateSecretFixture() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
		"metadata": map[string]any{
			"name": "webhook-cert", "namespace": "operator", "uid": "original-uid", "resourceVersion": "original",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "Helm", "operator.ptah.run/generated-webhook-certificate": "true",
			},
			"annotations": map[string]any{
				"meta.helm.sh/release-name": "ptah", "meta.helm.sh/release-namespace": "operator",
			},
		},
		"data": map[string]any{
			"ca.crt": "CA-FIXTURE", "ca.key": "CA-PRIVATE-KEY-FIXTURE",
			"tls.crt": "CERT-FIXTURE", "tls.key": "TLS-PRIVATE-KEY-FIXTURE",
		},
	}
}
