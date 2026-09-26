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

func TestCertificateE2EParkedWebhookInventory(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"mutatingwebhookconfiguration", "validatingwebhookconfiguration"} {
		for _, scenario := range []string{"parked", "foreign namespace", "foreign service", "extra webhook", "missing canary", "unparked canary"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				fixture := newCertificateShellFixture(t)
				entries := [][3]string{
					{"mapproval.operator.ptah.run", "webhook", "/mutate-operator-ptah-run-v1alpha1-ptahschemaapproval"},
					{"mmigrationapproval.operator.ptah.run", "webhook", "/mutate-operator-ptah-run-v1alpha1-ptahmigrationapproval"},
					{"certificate-rotation-canary-mutate.operator.ptah.run", "candidate", "/candidate/mutate"},
				}
				if kind == "validatingwebhookconfiguration" {
					entries = [][3]string{
						{"vapproval.operator.ptah.run", "webhook", "/validate-operator-ptah-run-v1alpha1-ptahschemaapproval"},
						{"vmigrationapproval.operator.ptah.run", "webhook", "/validate-operator-ptah-run-v1alpha1-ptahmigrationapproval"},
						{"vpodintent.operator.ptah.run", "webhook", "/validate-v1-pod-ptah-operation-intent"},
						{"vcontrollerwrite.operator.ptah.run", "webhook", "/validate-operator-controller-write"},
						{"certificate-rotation-canary-validate.operator.ptah.run", "candidate", "/candidate/validate"},
					}
				}
				webhooks := make([]map[string]any, 0, len(entries))
				for _, entry := range entries {
					webhooks = append(webhooks, map[string]any{"name": entry[0], "clientConfig": map[string]any{
						"caBundle": "parked-ca", "service": map[string]any{
							"name": entry[1], "namespace": "operator", "port": 443, "path": entry[2],
						},
					}})
				}
				canary := webhooks[len(webhooks)-1]["clientConfig"].(map[string]any)
				switch scenario {
				case "foreign namespace":
					canary["service"].(map[string]any)["namespace"] = "foreign"
				case "foreign service":
					canary["service"].(map[string]any)["name"] = "foreign"
				case "extra webhook":
					webhooks = append(webhooks, webhooks[0])
				case "missing canary":
					webhooks = webhooks[:len(webhooks)-1]
				case "unparked canary":
					canary["caBundle"] = "proof-ca"
				}
				fixture.writeJSON("api-secret.json", map[string]any{"webhooks": webhooks})
				output, err := fixture.run("uniform_service_bundle " + kind + " admission\n")
				if err != nil {
					t.Fatalf("bundle inspection: %v\n%s", err, output)
				}
				if (strings.TrimSpace(output) == "parked-ca") != (scenario == "parked") {
					t.Fatalf("unexpected bundle result %q", output)
				}
			})
		}
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
  [ "$argument" = get ] && exec cat "$UPGRADE_WORK_DIR/api-secret.json"
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
CANDIDATE_SERVICE=candidate
SECRET_BEFORE=$UPGRADE_WORK_DIR/secret-before.json
SECRET_ERROR=$UPGRADE_WORK_DIR/secret-error.log
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
