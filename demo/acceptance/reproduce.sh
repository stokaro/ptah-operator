#!/bin/sh
# Repeat a published scenario with none of this repository's scripts.
#
# A demonstration is a claim about the product, so the claim has to survive the
# demonstration being taken away. This runs the commands the first scenario
# publishes, from a directory where demo/bin/lab does not exist, against a
# ServiceAccount that may not create a Job, and asserts the database converged.
#
# It measures two claims, and one of them is not about the demonstration:
#
#   - what a scenario shows is kubectl, ptah and a manifest, and nothing of
#     ours is needed to repeat it;
#   - the executor's Job is the operator's to create. A reader who cannot
#     create one still gets a converged database, which is the difference
#     between an operator and a script that happens to run in a cluster.
#
# The lab is still allowed to hand over what it generated -- the cluster, the
# registry and its credentials -- because that is the environment a reader
# brings. What it may not do is take part in the scenario.
set -eu

ROOT_DIR=$(cd "$(dirname "$0")/../.." && pwd)
LAB_ENVIRONMENT=${LAB_ENVIRONMENT:-$ROOT_DIR/demo/.lab/environment}
ACCOUNT=demo-repeater
SCHEMA=storefront

fail() {
	printf 'reproduce: %s\n' "$1" >&2
	exit 1
}

say() {
	printf 'reproduce: %s\n' "$1"
}

[ -f "$LAB_ENVIRONMENT" ] ||
	fail "no lab environment at $LAB_ENVIRONMENT; bring the lab up first: make demo-up"

set -a
# shellcheck disable=SC1090 # The path is the lab's, and it is a NAME=value file.
. "$LAB_ENVIRONMENT"
set +a

[ -n "${E2E_KUBECONFIG:-}" ] || fail 'the lab environment carries no E2E_KUBECONFIG'
[ -n "${E2E_TEST_NAMESPACE:-}" ] || fail 'the lab environment carries no E2E_TEST_NAMESPACE'

NAMESPACE=$E2E_TEST_NAMESPACE
ADMIN_KUBECONFIG=$E2E_KUBECONFIG
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-reproduce.XXXXXX")

admin() {
	kubectl --kubeconfig "$ADMIN_KUBECONFIG" "$@"
}

cleanup() {
	admin -n "$NAMESPACE" delete ptahschema "$SCHEMA" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete rolebinding "$ACCOUNT" --ignore-not-found >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete role "$ACCOUNT" --ignore-not-found >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete serviceaccount "$ACCOUNT" --ignore-not-found >/dev/null 2>&1 || true
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Preparation. This is the part a reader does with their own cluster, their own
# registry and their own credentials, so here it is the lab handing over what
# it generated.

say 'preparing: reading the lab environment'
eval "$("$ROOT_DIR/demo/bin/lab" credentials)"
export PTAH_OCI_USERNAME PTAH_OCI_PASSWORD PTAH_OCI_REGISTRY
PATH="$("$ROOT_DIR/demo/bin/lab" ptah):$PATH"
export PATH
[ -n "${E2E_REGISTRY_HOST:-}" ] || fail 'the lab environment carries no E2E_REGISTRY_HOST'
REGISTRY_IN_CLUSTER=$E2E_REGISTRY_HOST
"$ROOT_DIR/demo/bin/lab" reset

# The account the reproduction runs as. It may do what a person operating a
# schema does, and it may read the Jobs the operator creates. It may not create
# one: that verb is deliberately absent below, and the reproduction checks the
# absence before it starts rather than trusting this text.
say "preparing: the $ACCOUNT account, without the verb that creates a Job"
admin -n "$NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $ACCOUNT
  namespace: $NAMESPACE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: $ACCOUNT
  namespace: $NAMESPACE
rules:
  - apiGroups: [operator.ptah.dev]
    resources: [ptahschemas, ptahschemaplans, ptahschemaapprovals]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [operator.ptah.dev]
    resources: [ptahschemas/status, ptahschemaplans/status, ptahschemaapprovals/status]
    verbs: [get]
  - apiGroups: [""]
    resources: [configmaps, pods]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods/exec]
    verbs: [create]
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get, list]
  - apiGroups: [batch]
    resources: [jobs]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: $ACCOUNT
  namespace: $NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: $ACCOUNT
subjects:
  - kind: ServiceAccount
    name: $ACCOUNT
    namespace: $NAMESPACE
YAML

TOKEN=$(admin -n "$NAMESPACE" create token "$ACCOUNT" --duration=60m)
SERVER=$(admin config view --minify -o jsonpath='{.clusters[0].cluster.server}')
AUTHORITY=$(admin config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[ -n "$SERVER" ] || fail 'the lab kubeconfig names no API server'
[ -n "$AUTHORITY" ] || fail 'the lab kubeconfig carries no cluster certificate authority'

cat >"$WORK_DIR/kubeconfig" <<YAML
apiVersion: v1
kind: Config
clusters:
  - name: lab
    cluster:
      server: $SERVER
      certificate-authority-data: $AUTHORITY
users:
  - name: $ACCOUNT
    user:
      token: $TOKEN
contexts:
  - name: lab
    context:
      cluster: lab
      user: $ACCOUNT
      namespace: $NAMESPACE
current-context: lab
YAML
chmod 600 "$WORK_DIR/kubeconfig"

# ---------------------------------------------------------------------------
# The reproduction. From here nothing of this repository's is reachable: the
# working directory holds the two files a reader would have, and demo/bin/lab
# is not one of them.

mkdir -p "$WORK_DIR/repeat"
cp "$ROOT_DIR/demo/schemas/v1.sql" "$WORK_DIR/repeat/schema.sql"
cp "$ROOT_DIR/demo/manifests/storefront.yaml" "$WORK_DIR/repeat/storefront.yaml"
cd "$WORK_DIR/repeat"

if [ -e demo/bin/lab ]; then
	fail 'the lab script is reachable from the reproduction, so this proved nothing'
fi
if command -v lab >/dev/null 2>&1; then
	fail 'a lab command is on PATH, so this proved nothing'
fi

export KUBECONFIG="$WORK_DIR/kubeconfig"

say 'checking: this account may not create a Job'
if kubectl -n "$NAMESPACE" auth can-i create jobs >/dev/null; then
	fail 'the account may create a Job, so the run that follows would prove nothing'
fi
kubectl -n "$NAMESPACE" auth can-i create ptahschemas.operator.ptah.dev >/dev/null ||
	fail 'the account may not create a PtahSchema, so it cannot operate a schema at all'

say 'publishing the desired schema as an artifact'
DIGEST=$(ptah schema push "oci://$PTAH_OCI_REGISTRY/schemas/demo:repeat-$$" \
	--schema-file schema.sql --dialect postgres --plain-http |
	sed -n 's/^Digest: //p')
case "$DIGEST" in
sha256:*) ;;
*) fail "the push returned no digest, and a tag is not what the operator is pointed at" ;;
esac

say "pointing the operator at $DIGEST"
sed -i.template \
	-e "s|\${NAMESPACE}|$NAMESPACE|g" \
	-e "s|\${REGISTRY}|$REGISTRY_IN_CLUSTER|g" \
	-e "s|\${DIGEST}|$DIGEST|g" \
	-e "s|\${APPLY}|Always|g" \
	-e "s|\${ALLOW_DESTRUCTIVE}|false|g" \
	-e "s|\${INTERVAL}|1m|g" \
	-e "s|\${SUSPEND}|false|g" \
	storefront.yaml
kubectl apply -f storefront.yaml >/dev/null

say 'waiting for the operator to converge the database'
kubectl -n "$NAMESPACE" wait --for=condition=InSync --timeout=600s "ptahschema/$SCHEMA" >/dev/null ||
	fail 'the schema did not reach InSync'

REASON=$(kubectl -n "$NAMESPACE" get "ptahschema/$SCHEMA" \
	-o jsonpath='{.status.conditions[?(@.type=="InSync")].reason}')
[ "$REASON" = "ScopedConverged" ] ||
	fail "InSync carries the reason $REASON, and convergence is ScopedConverged"

say 'reading the live database'
kubectl -n "$NAMESPACE" exec deploy/demo-psql -- psql -c '\d customers' | grep -q email ||
	fail 'the customers table has no email column, so the schema did not reach the database'

# A Job ran for this schema, and this account could not have started it. Both
# halves are the point: an operator did the work its caller was not permitted to
# do. The Jobs are counted by what owns them, not by what the namespace holds,
# because a namespace holds whatever an earlier run left behind.
APPLIED=$(kubectl -n "$NAMESPACE" get "ptahschema/$SCHEMA" \
	-o jsonpath='{.status.conditions[?(@.type=="Applying")].reason}')
[ "$APPLIED" = "JobCompleted" ] ||
	fail "Applying carries the reason $APPLIED, and a finished executor Job is JobCompleted"

SCHEMA_UID=$(kubectl -n "$NAMESPACE" get "ptahschema/$SCHEMA" -o jsonpath='{.metadata.uid}')
JOBS=$(kubectl -n "$NAMESPACE" get jobs -o json |
	jq --arg uid "$SCHEMA_UID" '[.items[] | select(any(.metadata.ownerReferences[]?; .uid == $uid))] | length')
[ "$JOBS" -ge 1 ] ||
	fail 'no executor Job belongs to this schema, so the convergence came from somewhere unexpected'
if kubectl -n "$NAMESPACE" auth can-i create jobs >/dev/null; then
	fail 'the account gained the verb that creates a Job during the run'
fi

say "repeated the scenario with no script of ours, under an account that may not create a Job ($JOBS of its Jobs ran)"
