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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
)

// These white-box tests execute the unexported shell functions verbatim. Docker
// is replaced only at the process boundary; this proves harness behavior and
// request identity, not authorization or informer convergence in a live API.
func TestHookProgressAuthorizationBarrier(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required to exercise hook progress authorization")
	}
	for _, test := range []struct {
		name, mode, wantError string
		budget                int
		wantSweeps            int
	}{
		{name: "all exact grants", budget: 90, wantSweeps: 1},
		{name: "honest no converges to yes", mode: "no-then-yes", budget: 2, wantSweeps: 2},
		{name: "one stale API times out", mode: "stale-api", budget: 2, wantError: "did not converge on all three API servers"},
		{name: "query error is not a denial", mode: "query-error", budget: 2, wantError: "query failed"},
		{name: "malformed JSON", mode: "malformed", budget: 2, wantError: "response is malformed"},
		{name: "nonboolean answer", mode: "nonboolean", budget: 2, wantError: "response is malformed"},
		{name: "missing answer", mode: "missing", budget: 2, wantError: "response is malformed"},
		{name: "evaluation error", mode: "evaluation-error", budget: 2, wantError: "response is malformed"},
		{name: "contradictory answer", mode: "contradictory", budget: 2, wantError: "response is malformed"},
		{name: "explicit null denial", mode: "null-denial", budget: 2, wantError: "response is malformed"},
		{name: "explicit null evaluation error", mode: "null-evaluation", budget: 2, wantError: "response is malformed"},
		{name: "multiple response objects", mode: "multiple", budget: 2, wantError: "response is malformed"},
		{name: "unexpected extra grant on third API", mode: "extra-grant", budget: 2, wantError: "unexpected create jobs grant at 10.0.0.3"},
		{name: "late successful reply", mode: "late-reply", budget: 2, wantError: "exceeded its aggregate deadline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newHookAuthorizationFixture(t)
			fixture.mode = test.mode
			fixture.budget = test.budget
			output, err := fixture.run(t)
			if test.wantError != "" {
				if err == nil || !strings.Contains(string(output), test.wantError) {
					t.Fatalf("barrier error = %v, output = %s, want %q", err, output, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("barrier: %v: %s", err, output)
			}
			observations := fixture.observations(t)
			if len(observations) != 3*13*test.wantSweeps {
				t.Fatalf("authorization requests = %d, want %d complete three-API sweeps", len(observations), test.wantSweeps)
			}
			for sweep := range test.wantSweeps {
				assertHookAuthorizationSweep(t, observations[sweep*39:(sweep+1)*39], min(test.budget-sweep, 15))
			}
		})
	}
}

func TestHookProgressAuthorizationRejectsUnboundInputs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, wantError string
		mutate          func(*hookAuthorizationFixture)
	}{
		{name: "missing UID", wantError: "UID is missing", mutate: func(f *hookAuthorizationFixture) { f.uid = "" }},
		{name: "missing Docker context", wantError: "explicit Docker context", mutate: func(f *hookAuthorizationFixture) { f.dockerContext = "" }},
		{name: "arbitrary container target", wantError: "DNS-label kind cluster name", mutate: func(f *hookAuthorizationFixture) { f.cluster = "../foreign" }},
		{name: "wrong container name", wantError: "does not match the exact primary Node", mutate: func(f *hookAuthorizationFixture) { f.container["name"] = "/foreign-control-plane" }},
		{name: "wrong container IP", wantError: "does not match the exact primary Node", mutate: func(f *hookAuthorizationFixture) { f.container["address"] = "10.0.0.9" }},
		{name: "short container ID", wantError: "does not match the exact primary Node", mutate: func(f *hookAuthorizationFixture) { f.container["id"] = "abc123" }},
		{name: "missing API", wantError: "exact three control-plane endpoints", mutate: func(f *hookAuthorizationFixture) { f.endpoints = f.endpoints[:2] }},
		{name: "duplicate API", wantError: "exact three control-plane endpoints", mutate: func(f *hookAuthorizationFixture) { f.endpoints[2] = f.endpoints[1] }},
		{name: "unbound endpoint", wantError: "exact three control-plane endpoints", mutate: func(f *hookAuthorizationFixture) { f.endpoints[2] = "10.0.0.9" }},
		{name: "inventory symlink", wantError: "regular non-symlink file", mutate: func(f *hookAuthorizationFixture) { f.symlink = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newHookAuthorizationFixture(t)
			test.mutate(&fixture)
			output, err := fixture.run(t)
			if err == nil || !strings.Contains(string(output), test.wantError) {
				t.Fatalf("barrier error = %v, output = %s, want %q", err, output, test.wantError)
			}
			if got := len(fixture.observations(t)); got != 0 {
				t.Fatalf("unbound fixture sent %d authorization queries", got)
			}
		})
	}
}

func TestHookProgressAuthorizationRejectsContractBypasses(t *testing.T) {
	t.Parallel()
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	if err := verifyHookProgressAuthorizationSource("fixture.sh", []byte(source)); err != nil {
		t.Fatalf("unmodified authorization contract: %v", err)
	}
	for _, test := range []struct{ name, function, old, replacement string }{
		{"missing barrier", "create_hook_progress_adversary_and_hold", "\twait_for_hook_progress_authorization\n", ""},
		{"barrier before UID", "create_hook_progress_adversary_and_hold", "\t[ -n \"$HOOK_PROGRESS_ADVERSARY_UID\" ] || fail \"hook progress adversary has no UID\"\n\twait_for_hook_progress_authorization\n", "\twait_for_hook_progress_authorization\n\t[ -n \"$HOOK_PROGRESS_ADVERSARY_UID\" ] || fail \"hook progress adversary has no UID\"\n"},
		{"UID bypass", "expect_hook_progress_authorization", `--as-uid "$HOOK_PROGRESS_ADVERSARY_UID"`, `--as-uid other-uid`},
		{"username bypass", "expect_hook_progress_authorization", `--as "system:serviceaccount:$E2E_OPERATOR_NAMESPACE:$HOOK_PROGRESS_ADVERSARY"`, `--as system:admin`},
		{"namespace bypass", "expect_hook_progress_authorization", `hook_auth_namespace=$E2E_OPERATOR_NAMESPACE`, `hook_auth_namespace=default`},
		{"API group bypass", "expect_hook_progress_authorization", `jobs) hook_auth_group='batch' ;;`, `jobs) hook_auth_group= ;;`},
		{"subresource bypass", "expect_hook_progress_authorization", `*/*) hook_auth_subresource=${3#*/} ;;`, `*/*) hook_auth_subresource= ;;`},
		{"load balancer substitution", "expect_hook_progress_authorization", `--server "https://${HOOK_PROGRESS_AUTHORIZATION_ENDPOINT}:6443"`, `--server "$LOAD_BALANCER"`},
		{"arbitrary Docker target", "expect_hook_progress_authorization", `exec -i "$HOOK_PROGRESS_AUTHORIZATION_CONTAINER_ID"`, `exec -i "$EXTRA_DOCKER_TARGET"`},
		{"uncapped request", "expect_hook_progress_authorization", `[ "$hook_auth_remaining" -le 15 ] || hook_auth_remaining=15`, `hook_auth_remaining=90`},
		{"discard API error", "expect_hook_progress_authorization", `fail "hook progress authorization query failed`, `printf '%s\n' "hook progress authorization query failed`},
		{"multiple JSON acceptance", "expect_hook_progress_authorization", `select(length == 1) | .[0] |`, `.[0] |`},
		{"ignore evaluation error", "expect_hook_progress_authorization", `((has("evaluationError") | not) or .evaluationError == "") and`, `true and`},
		{"missing endpoint validation", "prepare_hook_progress_authorization_endpoints", `-f "$ROOT_DIR/hack/api-server-endpoint-inventory.jq"`, `-f /tmp/unrelated.jq`},
		{"wrong container identity", "prepare_hook_progress_authorization_endpoints", `.name == $name and .address == $address and`, `.name == $name and`},
		{"missing preread grant", "wait_for_hook_progress_authorization", `'delete jobs' 'get jobs'`, `'delete jobs'`},
		{"missing negative", "wait_for_hook_progress_authorization", `'create jobs' 'update jobs' 'update pods'`, `'create jobs' 'update jobs'`},
		{"skip third API", "wait_for_hook_progress_authorization", `[ "$hook_auth_endpoint_count" -eq 3 ]`, `[ "$hook_auth_endpoint_count" -eq 2 ]`},
		{"ignore stale API", "wait_for_hook_progress_authorization", `hook_auth_ready=0`, `hook_auth_ready=1`},
		{"reset aggregate deadline", "wait_for_hook_progress_authorization", "\twhile [ \"$(date +%s)\" -lt \"$HOOK_PROGRESS_AUTHORIZATION_DEADLINE\" ]; do\n", "\twhile [ \"$(date +%s)\" -lt \"$HOOK_PROGRESS_AUTHORIZATION_DEADLINE\" ]; do\n\t\tHOOK_PROGRESS_AUTHORIZATION_DEADLINE=$(($(date +%s) + HOOK_PROGRESS_AUTHORIZATION_SECONDS))\n"},
		{"early success", "wait_for_hook_progress_authorization", "wait_for_hook_progress_authorization() {\n", "wait_for_hook_progress_authorization() {\n\treturn 0\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := extractE2EShellFunction(t, source, test.function)
			if !strings.Contains(body, test.old) {
				t.Fatalf("mutation marker %q is missing", test.old)
			}
			mutated := strings.Replace(source, body, strings.Replace(body, test.old, test.replacement, 1), 1)
			if err := verifyHookProgressAuthorizationSource("fixture.sh", []byte(mutated)); err == nil {
				t.Fatal("authorization contract accepted the bypass")
			}
		})
	}
}

func TestHookProgressAuthorizationRejectsParentWiringBypass(t *testing.T) {
	t.Parallel()
	for _, binding := range []string{
		"E2E_KIND_CLUSTER_NAME=$CLUSTER_NAME \\\n",
		"E2E_API_SERVER_NODE_INVENTORY_FILE=$NODE_READINESS_FILE \\\n",
		"E2E_API_SERVER_ENDPOINT_INVENTORY_FILE=$API_SERVER_ENDPOINT_INVENTORY_FILE \\\n",
	} {
		t.Run(strings.SplitN(binding, "=", 2)[0], func(t *testing.T) {
			t.Parallel()
			files := repositoryE2EWiringFiles()
			source := readE2ESource(t, files.harness)
			beforePhase, _, found := strings.Cut(source, "E2E_PHASE=upgrade \\\n")
			start := strings.LastIndex(beforePhase, "E2E_KUBECONFIG=$KUBECONFIG_FILE \\\n")
			if !found || start < 0 {
				t.Fatal("upgrade invocation is missing")
			}
			upgrade := beforePhase[start:]
			if strings.Count(upgrade, binding) != 1 {
				t.Fatal("upgrade authorization input is not unique")
			}
			files.harness = writeMutatedE2ESource(t, "e2e-kind.sh", source, upgrade, strings.Replace(upgrade, binding, "", 1))
			if err := verifyE2EWiring(files); err == nil || !strings.Contains(err.Error(), "candidate upgrade lifecycle") {
				t.Fatalf("parent wiring error = %v, want missing upgrade input", err)
			}
		})
	}
}

type hookAuthorizationFixture struct {
	dir, root, cluster, uid, dockerContext, mode string
	budget                                       int
	container                                    map[string]any
	endpoints                                    []string
	symlink                                      bool
}

func newHookAuthorizationFixture(t *testing.T) hookAuthorizationFixture {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return hookAuthorizationFixture{
		dir: t.TempDir(), root: root, cluster: "test", uid: "fixture-sa-uid", dockerContext: "fixture-context", budget: 2,
		container: map[string]any{"id": strings.Repeat("a", 64), "name": "/test-control-plane", "address": "10.0.0.1"},
		endpoints: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
	}
}

func (f hookAuthorizationFixture) run(t *testing.T) ([]byte, error) {
	t.Helper()
	nodes := make([]any, 0, 3)
	for index, suffix := range []string{"", "2", "3"} {
		nodes = append(nodes, map[string]any{
			"metadata": map[string]any{"name": "test-control-plane" + suffix, "labels": map[string]string{"node-role.kubernetes.io/control-plane": ""}},
			"status":   map[string]any{"addresses": []any{map[string]string{"type": "InternalIP", "address": fmt.Sprintf("10.0.0.%d", index+1)}}},
		})
	}
	endpoints := make([]any, 0, len(f.endpoints))
	for _, address := range f.endpoints {
		endpoints = append(endpoints, map[string]any{"addresses": []string{address}, "conditions": map[string]bool{"ready": true}})
	}
	for name, value := range map[string]any{
		"nodes.json": map[string]any{"items": nodes},
		"slices.json": map[string]any{"items": []any{map[string]any{
			"metadata": map[string]any{"labels": map[string]string{"kubernetes.io/service-name": "kubernetes"}}, "addressType": "IPv4",
			"ports": []any{map[string]any{"name": "https", "protocol": "TCP", "port": 6443}}, "endpoints": endpoints,
		}}},
		"container.json": f.container,
	} {
		contents, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nodeFile := filepath.Join(f.dir, "nodes.json")
	if f.symlink {
		nodeFile = filepath.Join(f.dir, "nodes-link.json")
		if err := os.Symlink(filepath.Join(f.dir, "nodes.json"), nodeFile); err != nil {
			t.Fatal(err)
		}
	}
	source := readE2ESource(t, repositoryE2EWiringFiles().crdUpgrade)
	var script strings.Builder
	script.WriteString("set -eu\numask 077\n")
	for _, name := range []string{"fail", "require_mode_0600_regular_file", "prepare_hook_progress_authorization_endpoints", "expect_hook_progress_authorization", "wait_for_hook_progress_authorization"} {
		script.WriteString(extractE2EShellFunction(t, source, name) + "\n")
	}
	script.WriteString(hookAuthorizationMock + "\nwait_for_hook_progress_authorization\n")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "sh", "-c", script.String())
	command.Env = append(os.Environ(),
		"WORK_DIR="+f.dir, "ROOT_DIR="+f.root, "E2E_KIND_CLUSTER_NAME="+f.cluster, "E2E_DOCKER_CONTEXT="+f.dockerContext,
		"E2E_API_SERVER_NODE_INVENTORY_FILE="+nodeFile, "E2E_API_SERVER_ENDPOINT_INVENTORY_FILE="+filepath.Join(f.dir, "slices.json"),
		"E2E_OPERATOR_NAMESPACE=fixture-system", "HOOK_PROGRESS_ADVERSARY=fixture-adversary", "HOOK_PROGRESS_ADVERSARY_UID="+f.uid,
		"HOOK_PROGRESS_AUTHORIZATION_ENDPOINTS="+filepath.Join(f.dir, "authorization-endpoints"),
		fmt.Sprintf("HOOK_PROGRESS_AUTHORIZATION_SECONDS=%d", f.budget), "MOCK_MODE="+f.mode,
	)
	return command.CombinedOutput()
}

const hookAuthorizationMock = `
printf '0\n' >"$WORK_DIR/clock"
date() { command cat "$WORK_DIR/clock"; }
sleep() { printf '%s\n' "$(($(command cat "$WORK_DIR/clock") + 1))" >"$WORK_DIR/clock"; }
docker() {
  if [ "$3" = container ]; then
    command cat "$WORK_DIR/container.json"
    return
  fi
  mock_request=$(command cat)
  jq -nc --argjson request "$mock_request" '{args:$ARGS.positional,request:$request}' --args -- "$@" >>"$WORK_DIR/queries.jsonl" || return 99
  mock_verb=$(printf '%s' "$mock_request" | jq -r '.spec.resourceAttributes.verb')
  mock_resource=$(printf '%s' "$mock_request" | jq -r '.spec.resourceAttributes.resource')
  mock_allowed=true
  case "$mock_verb" in create|update) mock_allowed=false ;; esac
  case "$MOCK_MODE" in
    query-error) return 7 ;;
    malformed) printf 'not JSON\n'; return ;;
    nonboolean) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":"true"}}'; return ;;
    missing) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{}}'; return ;;
    evaluation-error) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true,"evaluationError":"fixture-error"}}'; return ;;
    contradictory) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true,"denied":true}}'; return ;;
    null-denial) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true,"denied":null}}'; return ;;
    null-evaluation) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true,"evaluationError":null}}'; return ;;
    multiple) printf '%s\n' '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true}}' '{"kind":"Status"}'; return ;;
    late-reply) printf '%s\n' "$HOOK_PROGRESS_AUTHORIZATION_SECONDS" >"$WORK_DIR/clock" ;;
  esac
  case " $* " in
    *' --server https://10.0.0.2:6443 '*)
      if [ "$mock_verb" = delete ]; then
        case "$MOCK_MODE" in
          stale-api) mock_allowed=false ;;
          no-then-yes) if [ ! -f "$WORK_DIR/denied-once" ]; then : >"$WORK_DIR/denied-once"; mock_allowed=false; fi ;;
        esac
      fi ;;
    *' --server https://10.0.0.3:6443 '*)
      if [ "$MOCK_MODE" = extra-grant ] && [ "$mock_verb" = create ] && [ "$mock_resource" = jobs ]; then mock_allowed=true; fi ;;
  esac
  if [ "$MOCK_MODE" = no-then-yes ]; then
    printf '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":%s,"denied":false}}\n' "$mock_allowed"
  else
    printf '{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":%s}}\n' "$mock_allowed"
  fi
}
`

type hookAuthorizationObservation struct {
	Args    []string                                `json:"args"`
	Request authorizationv1.SelfSubjectAccessReview `json:"request"`
}

func (f hookAuthorizationFixture) observations(t *testing.T) []hookAuthorizationObservation {
	t.Helper()
	file, err := os.Open(filepath.Join(f.dir, "queries.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	var observations []hookAuthorizationObservation
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var observation hookAuthorizationObservation
		if err := json.Unmarshal(scanner.Bytes(), &observation); err != nil {
			t.Fatal(err)
		}
		observations = append(observations, observation)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return observations
}

func assertHookAuthorizationSweep(t *testing.T, observations []hookAuthorizationObservation, timeout int) {
	t.Helper()
	capabilities := []string{
		"delete batch jobs", "get batch jobs", "get batch jobs/status", "patch batch jobs/status",
		"get  pods", "patch  pods", "get  pods/status", "patch  pods/status",
		"create admissionregistration.k8s.io validatingadmissionpolicies", "create admissionregistration.k8s.io validatingadmissionpolicybindings",
		"create batch jobs", "update batch jobs", "update  pods",
	}
	for index, observation := range observations {
		endpoint := fmt.Sprintf("https://10.0.0.%d:6443", index/13+1)
		wantArgs := []string{"--context", "fixture-context", "exec", "-i", strings.Repeat("a", 64), "kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf",
			"--server", endpoint, "--tls-server-name", "kubernetes", "--as", "system:serviceaccount:fixture-system:fixture-adversary",
			"--as-uid", "fixture-sa-uid", "--as-group", "system:serviceaccounts", "--as-group", "system:serviceaccounts:fixture-system",
			"--as-group", "system:authenticated", fmt.Sprintf("--request-timeout=%ds", timeout),
			"create", "--raw", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", "-f", "-"}
		if !slices.Equal(observation.Args, wantArgs) {
			t.Fatalf("query %d arguments = %q, want %q", index, observation.Args, wantArgs)
		}
		attributes := observation.Request.Spec.ResourceAttributes
		wantNamespace := "fixture-system"
		if index%13 == 8 || index%13 == 9 {
			wantNamespace = ""
		}
		if observation.Request.APIVersion != "authorization.k8s.io/v1" || observation.Request.Kind != "SelfSubjectAccessReview" || attributes == nil || attributes.Namespace != wantNamespace {
			t.Fatalf("query %d has incorrect type or namespace: %+v", index, observation.Request)
		}
		resource := attributes.Resource
		if attributes.Subresource != "" {
			resource += "/" + attributes.Subresource
		}
		if capability := attributes.Verb + " " + attributes.Group + " " + resource; capability != capabilities[index%13] {
			t.Fatalf("query %d capability = %q, want %q", index, capability, capabilities[index%13])
		}
	}
}
