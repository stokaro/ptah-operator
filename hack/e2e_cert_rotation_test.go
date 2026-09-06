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

func TestCertificateE2ELegacySecretRestoration(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{
		"lost removal response", "lost restoration response", "already restored",
		"concurrent data change", "replacement identity", "foreign ownership",
		"foreign type", "race after GET", "restoration rejected",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			original := certificateSecretFixture()
			fixture.writeJSON("legacy-secret-before.json", original)
			live := certificateSecretFixture()
			delete(live["data"].(map[string]any), "ca.key")
			live["metadata"].(map[string]any)["resourceVersion"] = "removed"
			fixture.writeJSON("legacy-secret-after-remove.json", live)
			wantRestore := true
			switch scenario {
			case "lost removal response":
				fixture.write("legacy-secret-after-remove.json", "")
			case "lost restoration response":
				fixture.mode = "lost-response"
			case "already restored":
				live = certificateSecretFixture()
				live["metadata"].(map[string]any)["resourceVersion"] = "restored-without-response"
			case "concurrent data change":
				live["data"].(map[string]any)["tls.crt"] = "concurrent-certificate"
				wantRestore = false
			case "replacement identity":
				live["metadata"].(map[string]any)["uid"] = "replacement"
				wantRestore = false
			case "foreign ownership":
				live["metadata"].(map[string]any)["annotations"].(map[string]any)["meta.helm.sh/release-name"] = "foreign"
				wantRestore = false
			case "foreign type":
				live["type"] = "Opaque"
				wantRestore = false
			case "race after GET":
				fixture.mode = "race"
				wantRestore = false
			case "restoration rejected":
				fixture.mode = "reject"
				wantRestore = false
			}
			fixture.writeJSON("api-secret.json", live)
			script := "restore_legacy_secret\n[ \"$LEGACY_SECRET_RESTORE_REQUIRED\" -eq 0 ]\n"
			if !wantRestore {
				script = "trap cleanup_upgrade_files EXIT\nexit 1\n"
			}
			output, err := fixture.run(script)
			if wantRestore && err != nil {
				t.Fatalf("restore failed: %v\n%s", err, output)
			}
			if !wantRestore && (err == nil || !strings.Contains(output, "protected recovery files retained at "+fixture.directory)) {
				t.Fatalf("unsafe recovery did not retain its backup: error %v, output %s", err, output)
			}
			contents, err := os.ReadFile(filepath.Join(fixture.directory, "api-secret.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(contents, &result); err != nil {
				t.Fatal(err)
			}
			_, hasKey := result["data"].(map[string]any)["ca.key"]
			if hasKey != wantRestore {
				t.Fatalf("restored key = %v, want %v", hasKey, wantRestore)
			}
			if !wantRestore {
				info, err := os.Stat(filepath.Join(fixture.directory, "legacy-secret-before.json"))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("protected backup lost: info %v, error %v", info, err)
				}
			}
			if strings.Contains(output, "PRIVATE-KEY-FIXTURE") {
				t.Fatal("restoration printed private key material")
			}
		})
	}
}

func TestCertificateE2ELegacySecretProofRejectsConcurrentRecovery(t *testing.T) {
	t.Parallel()
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same removal version", true: "rotator write during dry run"}[changed], func(t *testing.T) {
			t.Parallel()
			fixture := newCertificateShellFixture(t)
			fixture.writeJSON("legacy-secret-before.json", certificateSecretFixture())
			removed := certificateSecretFixture()
			delete(removed["data"].(map[string]any), "ca.key")
			removed["metadata"].(map[string]any)["resourceVersion"] = "removed"
			fixture.writeJSON("legacy-secret-after-remove.json", removed)
			if changed {
				removed["metadata"].(map[string]any)["resourceVersion"] = "concurrent-write"
			}
			fixture.writeJSON("api-secret.json", removed)
			output, err := fixture.run("verify_legacy_secret_lookup_state\n")
			if (err != nil) != changed {
				t.Fatalf("proof error = %v, changed = %v, output = %s", err, changed, output)
			}
		})
	}
}

func TestCertificateE2ELegacySecretRequiresExactSource(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"valid", "name", "namespace", "type", "labels", "annotations", "tls.key", "extra data"} {
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
			case "extra data":
				secret["data"].(map[string]any)["unexpected"] = "foreign"
			}
			fixture.writeJSON("legacy-secret-before.json", secret)
			output, err := fixture.run("validate_generated_secret \"$LEGACY_SECRET_BEFORE\" original\n")
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
					{"mapproval.operator.ptah.dev", "webhook", "/mutate-operator-ptah-dev-v1alpha1-ptahschemaapproval"},
					{"certificate-rotation-canary-mutate.operator.ptah.dev", "candidate", "/candidate/mutate"},
				}
				if kind == "validatingwebhookconfiguration" {
					entries = [][3]string{
						{"vapproval.operator.ptah.dev", "webhook", "/validate-operator-ptah-dev-v1alpha1-ptahschemaapproval"},
						{"vpodintent.operator.ptah.dev", "webhook", "/validate-v1-pod-ptah-operation-intent"},
						{"vcontrollerwrite.operator.ptah.dev", "webhook", "/validate-operator-controller-write"},
						{"certificate-rotation-canary-validate.operator.ptah.dev", "candidate", "/candidate/validate"},
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
	mode      string
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
verb=
patch_file=
while [ "$#" -gt 0 ]; do
  case "$1" in
    get|patch) verb=$1 ;;
    --patch-file) shift; patch_file=$1 ;;
  esac
  shift
done
if [ "$verb" = get ]; then
  cat "$UPGRADE_WORK_DIR/api-secret.json"
  exit 0
fi
[ "$verb" = patch ] || exit 2
[ "${RESTORE_TEST_MODE:-}" != reject ] || exit 1
if [ "${RESTORE_TEST_MODE:-}" = race ]; then
  jq '.metadata.resourceVersion = "raced" | .data["tls.crt"] = "concurrent-certificate"' \
    "$UPGRADE_WORK_DIR/api-secret.json" >"$UPGRADE_WORK_DIR/concurrent.json"
  mv "$UPGRADE_WORK_DIR/concurrent.json" "$UPGRADE_WORK_DIR/api-secret.json"
fi
jq -e --slurpfile patch "$patch_file" '
  reduce $patch[0][] as $operation (.;
    ($operation.path | split("/")[1:]) as $path |
    if $operation.op == "test" then
      if getpath($path) == $operation.value then . else error("patch precondition failed") end
    elif $operation.op == "add" then setpath($path; $operation.value)
    else error("unexpected patch operation") end) |
  .metadata.resourceVersion = "restored"
' "$UPGRADE_WORK_DIR/api-secret.json" >"$UPGRADE_WORK_DIR/patched.json"
mv "$UPGRADE_WORK_DIR/patched.json" "$UPGRADE_WORK_DIR/api-secret.json"
[ "${RESTORE_TEST_MODE:-}" != lost-response ] || exit 1
cat "$UPGRADE_WORK_DIR/api-secret.json"
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
LEGACY_SECRET_RESTORE_REQUIRED=1
LEGACY_SECRET_BEFORE=$UPGRADE_WORK_DIR/legacy-secret-before.json
LEGACY_SECRET_AFTER_REMOVE=$UPGRADE_WORK_DIR/legacy-secret-after-remove.json
LEGACY_SECRET_LIVE=$UPGRADE_WORK_DIR/legacy-secret-live.json
LEGACY_SECRET_VERIFIED=$UPGRADE_WORK_DIR/legacy-secret-verified.json
LEGACY_SECRET_RESTORE_PATCH=$UPGRADE_WORK_DIR/legacy-secret-restore-patch.json
LEGACY_SECRET_RESTORED=$UPGRADE_WORK_DIR/legacy-secret-restored.json
LEGACY_SECRET_ERROR=$UPGRADE_WORK_DIR/legacy-secret-error.log
. "$UPGRADE_WORK_DIR/functions.sh"
`+body)
	command := exec.Command("sh", filepath.Join(fixture.directory, "run.sh"))
	command.Env = append(os.Environ(), "UPGRADE_WORK_DIR="+fixture.directory,
		"PATH="+fixture.directory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+filepath.Dir(fixture.directory), "RESTORE_TEST_MODE="+fixture.mode)
	output, err := command.CombinedOutput()
	return string(output), err
}

func certificateSecretFixture() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
		"metadata": map[string]any{
			"name": "webhook-cert", "namespace": "operator", "uid": "original-uid", "resourceVersion": "original",
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "Helm", "operator.ptah.dev/generated-webhook-certificate": "true",
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
