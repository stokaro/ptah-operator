#!/bin/sh
# Repeat a published scenario with none of this repository's scripts.
#
# A demonstration is a claim about the product, so the claim has to survive the
# demonstration being taken away. This runs the commands the manual-approval
# scenario publishes, from a directory where demo/bin/lab does not exist,
# against a ServiceAccount that may not create a Job, and asserts the database
# converged. Then it does what the artifact scenarios publish: ptah schema push
# publishes the schema, a resource names it by digest, and kubectl ptah
# set-source moves that resource to the next revision, which is approved and
# applied the same way.
#
# It measures two claims, and one of them is not about the demonstration:
#
#   - what a scenario shows is kubectl, ptah and a manifest, and nothing of
#     ours is needed to repeat it;
#   - the executor's Job is the operator's to create. A reader who cannot
#     create one still gets a converged database, which is the difference
#     between an operator and a script that happens to run in a cluster.
#
# The resource asks for apply: OnApproval, which is the path the chart leaves
# open by default. Its apply-policy guard refuses apply: Always to anyone
# outside applyPolicyGuard.exemptGroups, and a ServiceAccount is in none of
# them, so a reader on a default install is on this path: apply, wait for the
# plan, have an approver approve that exact plan, and watch the operator apply
# it. The approver is a second ServiceAccount bound to the chart's approver
# ClusterRole. It may not create a PtahSchema or a Job, and the account that
# creates the schema may not approve, so the approval is made by an identity
# the request could not stand in for. It approves with kubectl ptah approve, as
# the scenario does; with no terminal to answer on, it names the fingerprint
# of the plan it read, which is how a script approves.
#
# The lab is still allowed to hand over what it generated -- the cluster, its
# Secrets, the two accounts -- because that is the environment a reader brings.
# What it may not do is take part in the scenario.
set -eu

ROOT_DIR=$(cd "$(dirname "$0")/../.." && pwd)
LAB_ENVIRONMENT=${LAB_ENVIRONMENT:-$ROOT_DIR/demo/.lab/environment}
ACCOUNT=demo-repeater
APPROVER=demo-approver
SCHEMA=storefront
APPROVAL=storefront-v1
NEXT_APPROVAL=storefront-v2

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
[ -n "${E2E_CONTROLLER_NAME:-}" ] || fail 'the lab environment carries no E2E_CONTROLLER_NAME'

NAMESPACE=$E2E_TEST_NAMESPACE
ADMIN_KUBECONFIG=$E2E_KUBECONFIG
# The chart names its approver ClusterRole after the release's fullname, which
# is also the controller Deployment's name.
APPROVER_CLUSTER_ROLE=$E2E_CONTROLLER_NAME-approver
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/ptah-reproduce.XXXXXX")

admin() {
	kubectl --kubeconfig "$ADMIN_KUBECONFIG" "$@"
}

cleanup() {
	admin -n "$NAMESPACE" delete ptahschema "$SCHEMA" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete ptahschemaapproval "$APPROVAL" "$NEXT_APPROVAL" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete configmap storefront-v1 --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete rolebinding "$ACCOUNT" "$APPROVER" --ignore-not-found >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete role "$ACCOUNT" --ignore-not-found >/dev/null 2>&1 || true
	admin -n "$NAMESPACE" delete serviceaccount "$ACCOUNT" "$APPROVER" --ignore-not-found >/dev/null 2>&1 || true
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Preparation. This is the part a reader does with their own cluster, their own
# registry and their own credentials, so here it is the lab handing over what
# it generated.

say 'preparing: reading the lab environment'
PATH="$("$ROOT_DIR/demo/bin/lab" tools):$PATH"
export PATH
"$ROOT_DIR/demo/bin/lab" reset

# The registry a push goes to and the account it pushes as, which ptah reads
# from the environment the way a CI job hands them over. The cluster pulls
# the same repository and digest from the address its Pods resolve.
"$ROOT_DIR/demo/bin/lab" credentials >"$WORK_DIR/credentials" ||
	fail 'the lab printed no registry credentials'
while IFS='=' read -r credential_name credential_value; do
	case "$credential_name" in
	PTAH_OCI_REGISTRY | PTAH_OCI_USERNAME | PTAH_OCI_PASSWORD)
		[ -n "$credential_value" ] || fail "the lab printed an empty $credential_name"
		export "$credential_name=$credential_value"
		;;
	esac
done <"$WORK_DIR/credentials"
rm -f "$WORK_DIR/credentials"
[ -n "${PTAH_OCI_REGISTRY:-}" ] && [ -n "${PTAH_OCI_USERNAME:-}" ] && [ -n "${PTAH_OCI_PASSWORD:-}" ] ||
	fail 'the lab printed no registry, username or password'
[ -n "${E2E_REGISTRY_HOST:-}" ] || fail 'the lab environment carries no E2E_REGISTRY_HOST'
REGISTRY_IN_CLUSTER=$E2E_REGISTRY_HOST

# The account the reproduction runs as. It may do what a person operating a
# schema does -- write the schema and the ConfigMap it reads its SQL from --
# and it may read the plans, the approvals and the Jobs the operator creates.
# It may not create a Job, and it may not approve: both verbs are deliberately
# absent below, and the reproduction checks the absence before it starts rather
# than trusting this text.
say "preparing: the $ACCOUNT account, without the verbs that create a Job or an approval"
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
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemas]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemaplans, ptahschemaplanchunks, ptahschemaapprovals]
    verbs: [get, list, watch]
  - apiGroups: [operator.ptah.run]
    resources: [ptahschemas/status, ptahschemaplans/status, ptahschemaapprovals/status]
    verbs: [get]
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get, create]
  - apiGroups: [""]
    resources: [pods]
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
  # Reading a plan needs the plan's own objects and nothing more: no Secret,
  # no ConfigMap, no Pod log, no exec, and nothing cluster-wide. The rule on
  # the plans above covers it, ptahschemaplanchunks being the plan's SQL.
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

# The account that approves. It gets the chart's own approver ClusterRole, in
# this namespace alone, which is how the chart says an approver is granted:
# it reads the resource, the plan and the approvals, and creates an approval.
# It has no verb on the desired state and none on a Job.
say "preparing: the $APPROVER account, bound to the chart's ClusterRole $APPROVER_CLUSTER_ROLE"
admin get clusterrole "$APPROVER_CLUSTER_ROLE" >/dev/null ||
	fail "the release installed no ClusterRole $APPROVER_CLUSTER_ROLE, which approverClusterRole.create renders"
admin -n "$NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $APPROVER
  namespace: $NAMESPACE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: $APPROVER
  namespace: $NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $APPROVER_CLUSTER_ROLE
subjects:
  - kind: ServiceAccount
    name: $APPROVER
    namespace: $NAMESPACE
YAML

SERVER=$(admin config view --minify -o jsonpath='{.clusters[0].cluster.server}')
AUTHORITY=$(admin config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[ -n "$SERVER" ] || fail 'the lab kubeconfig names no API server'
[ -n "$AUTHORITY" ] || fail 'the lab kubeconfig carries no cluster certificate authority'

# write_kubeconfig writes a kubeconfig for one of the two accounts: the lab's
# cluster, and a token that outlives the run by nothing it needs.
write_kubeconfig() {
	kubeconfig_account=$1
	kubeconfig_file=$2
	kubeconfig_token=$(admin -n "$NAMESPACE" create token "$kubeconfig_account" --duration=60m)
	cat >"$kubeconfig_file" <<YAML
apiVersion: v1
kind: Config
clusters:
  - name: lab
    cluster:
      server: $SERVER
      certificate-authority-data: $AUTHORITY
users:
  - name: $kubeconfig_account
    user:
      token: $kubeconfig_token
contexts:
  - name: lab
    context:
      cluster: lab
      user: $kubeconfig_account
      namespace: $NAMESPACE
current-context: lab
YAML
	chmod 600 "$kubeconfig_file"
}
write_kubeconfig "$ACCOUNT" "$WORK_DIR/kubeconfig"
write_kubeconfig "$APPROVER" "$WORK_DIR/approver-kubeconfig"

# ---------------------------------------------------------------------------
# The reproduction. From here nothing of this repository's is reachable: the
# working directory holds the two files a reader would have, and demo/bin/lab
# is not one of them.

mkdir -p "$WORK_DIR/repeat"
cp "$ROOT_DIR/demo/schemas/v1.sql" "$WORK_DIR/repeat/schema.sql"
cp "$ROOT_DIR/demo/schemas/v2.sql" "$WORK_DIR/repeat/schema-v2.sql"
cp "$ROOT_DIR/demo/manifests/manual-approval/storefront.yaml" "$WORK_DIR/repeat/storefront.yaml"
cd "$WORK_DIR/repeat"

if [ -e demo/bin/lab ]; then
	fail 'the lab script is reachable from the reproduction, so this proved nothing'
fi
if command -v lab >/dev/null 2>&1; then
	fail 'a lab command is on PATH, so this proved nothing'
fi

export KUBECONFIG="$WORK_DIR/kubeconfig"

# The approver's commands are the same kubectl, as the other account.
approver() {
	kubectl --kubeconfig "$WORK_DIR/approver-kubeconfig" "$@"
}

# fail_showing prints the schema's phase and conditions before failing, so a
# wait that ran out says what the operator was reporting when it did.
fail_showing() {
	kubectl -n "$NAMESPACE" get "ptahschema/$SCHEMA" -o json 2>/dev/null |
		jq -r '"reproduce: phase \(.status.phase // "-")",
			(.status.conditions[]? | "reproduce: \(.type)=\(.status) \(.reason): \(.message)")' >&2 || true
	fail "$1"
}

say 'checking: neither account may create a Job, and each may do only its own part'
if kubectl -n "$NAMESPACE" auth can-i create jobs >/dev/null; then
	fail 'the account may create a Job, so the run that follows would prove nothing'
fi
kubectl -n "$NAMESPACE" auth can-i create ptahschemas.operator.ptah.run >/dev/null ||
	fail 'the account may not create a PtahSchema, so it cannot operate a schema at all'
if kubectl -n "$NAMESPACE" auth can-i create ptahschemaapprovals.operator.ptah.run >/dev/null; then
	fail 'the account may approve its own request, so the approval below would decide nothing'
fi
if approver -n "$NAMESPACE" auth can-i create jobs >/dev/null; then
	fail 'the approver may create a Job, so the run that follows would prove nothing'
fi
if approver -n "$NAMESPACE" auth can-i create ptahschemas.operator.ptah.run >/dev/null; then
	fail 'the approver may create a PtahSchema, so the two accounts are not two parts'
fi
approver -n "$NAMESPACE" auth can-i create ptahschemaapprovals.operator.ptah.run >/dev/null ||
	fail 'the approver may not create an approval, so nothing below could approve the plan'

say 'putting the schema in a ConfigMap and applying the resource, with apply: OnApproval'
grep -q 'apply: OnApproval' storefront.yaml ||
	fail 'the manifest the scenario applies does not ask for an approval, so this would not be the default path'
kubectl -n "$NAMESPACE" create configmap storefront-v1 --from-file=schema.sql=schema.sql >/dev/null
kubectl -n "$NAMESPACE" apply -f storefront.yaml >/dev/null

# The operator plans and stops. The wait counts only a plan computed from a
# reading of the database that began after it did, and ends on the state the
# operator stays in until someone decides.
say 'waiting for the operator to plan, and to stop there'
kubectl ptah wait "schema/$SCHEMA" -n "$NAMESPACE" --for approval --timeout 10m >/dev/null ||
	fail_showing 'the schema did not come to wait for an approval'

# approve_current_plan has the approver read the plan and approve the plan it
# read: the fingerprint it names is the one the plan it printed carries, and
# approve refuses any other. No terminal answers the question approve asks a
# person, so the fingerprint is the decision, the way a script that reviewed
# the plan makes it. Admission stamps who approved, and that is read off the
# approval rather than off this script: it has to be the approver rather than
# the account that asked.
approve_current_plan() {
	approval_name=$1
	say "approving that exact plan, as $APPROVER"
	FINGERPRINT=$(approver -n "$NAMESPACE" get "ptahschema/$SCHEMA" -o jsonpath='{.status.plan.fingerprint}')
	case "$FINGERPRINT" in
	sha256:*) ;;
	*) fail 'the schema waits for an approval and names no plan fingerprint' ;;
	esac
	kubectl ptah plan "$SCHEMA" -n "$NAMESPACE" --kubeconfig "$WORK_DIR/approver-kubeconfig" >plan.txt ||
		fail 'the approver could not read the plan it is asked to approve'
	grep -qF "Fingerprint:    $FINGERPRINT" plan.txt ||
		fail "the plan the approver read is not the plan with fingerprint $FINGERPRINT"
	kubectl ptah approve "schema/$SCHEMA" -n "$NAMESPACE" --kubeconfig "$WORK_DIR/approver-kubeconfig" \
		--name "$approval_name" --plan-fingerprint "$FINGERPRINT" --wait --wait-timeout 10m >/dev/null ||
		fail_showing 'the approval was refused, or the plan it approved did not apply; approve says why above'
	STAMPED=$(kubectl -n "$NAMESPACE" get "ptahschemaapproval/$approval_name" -o jsonpath='{.spec.approver.username}')
	[ "$STAMPED" = "system:serviceaccount:$NAMESPACE:$APPROVER" ] ||
		fail "the approval records the approver as '$STAMPED', and the webhook stamps the identity that made the request"
}
approve_current_plan "$APPROVAL"

# approve --wait returned once the proof that reads the database after the
# Apply found it converged; that proof writes InSync in the same status.
REASON=$(kubectl -n "$NAMESPACE" get "ptahschema/$SCHEMA" \
	-o jsonpath='{.status.conditions[?(@.type=="InSync")].reason}')
[ "$REASON" = "ScopedConverged" ] ||
	fail "InSync carries the reason $REASON, and convergence is ScopedConverged"

say 'reading the live database'
kubectl -n "$NAMESPACE" exec deploy/demo-psql -- psql -c '\d customers' | grep -q email ||
	fail 'the customers table has no email column, so the schema did not reach the database'

# A Job ran for this schema, and neither account could have started it. Both
# halves are the point: an operator did the work its callers were not permitted
# to do. The Jobs are counted by what owns them, not by what the namespace
# holds, because a namespace holds whatever an earlier run left behind.
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
if approver -n "$NAMESPACE" auth can-i create jobs >/dev/null; then
	fail 'the approver gained the verb that creates a Job during the run'
fi

# The plan is read back through the client a reader installs, as the same
# account, with no more access than the rest of this run had.
say 'reading the applied plan with kubectl ptah'
kubectl ptah plan "$SCHEMA" --applied -n "$NAMESPACE" -o sql | grep -q 'CREATE TABLE' ||
	fail 'the applied plan does not carry the statement that created the table'
kubectl ptah plan "$SCHEMA" --applied -n "$NAMESPACE" -o json | grep -q '"statements"' ||
	fail 'the stored plan document did not come back as JSON'

# The Jobs go, and the plan stays readable. What a reader looks at afterwards is
# the plan the operator stored, not the pod that ran it: a Job that has been
# collected takes its logs with it and leaves the plan where it was.
say 'removing the executor Jobs the operator created'
admin -n "$NAMESPACE" delete jobs --all --ignore-not-found --timeout=120s >/dev/null
LEFT=$(kubectl -n "$NAMESPACE" get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -c . || true)
[ "$LEFT" -eq 0 ] || fail "the namespace still holds $LEFT Job(s), so the check below would prove nothing"
kubectl ptah plan "$SCHEMA" --applied -n "$NAMESPACE" -o sql | grep -q 'CREATE TABLE' ||
	fail 'the applied plan became unreadable once its Job was gone'

# ---------------------------------------------------------------------------
# The same schema from an artifact. A team that kept its schema in a ConfigMap
# moves to an artifact by applying the resource again with the artifact as its
# desired state: kubectl ptah set-source changes only the reference of a
# resource that already reads one, since an artifact also needs a verification
# policy, which only the person editing the resource names. The database holds
# that revision already, so the resource plans nothing, and the next revision
# is set-source's to point at.

# push_schema prints the reference the resource names: with --format
# reference, push prints what it published pinned by its digest, never the
# tag, under the address it pushed to, and the cluster pulls the same
# repository and digest from its own address.
push_schema() {
	push_file=$1
	push_tag=$2
	push_reference=$(ptah schema push "oci://$PTAH_OCI_REGISTRY/schemas/reproduce:$push_tag-$$" \
		--schema-file "$push_file" --dialect postgres --plain-http --format reference)
	case "$push_reference" in
	"oci://$PTAH_OCI_REGISTRY/schemas/reproduce@sha256:"*)
		printf 'oci://%s/%s\n' "$REGISTRY_IN_CLUSTER" "${push_reference#"oci://$PTAH_OCI_REGISTRY/"}"
		;;
	*) fail "ptah schema push printed no pinned reference for $push_file" ;;
	esac
}

say 'publishing the schema with ptah schema push'
FIRST=$(push_schema schema.sql v1)

say 'applying the resource again, reading that artifact instead of the ConfigMap'
kubectl -n "$NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchema
metadata:
  name: $SCHEMA
spec:
  target:
    engine: PostgreSQL
    coordinationKey: demo/storefront/primary
    urlFrom: {name: demo-database, key: url}
  desired:
    ociRef: $FIRST
    registryAuthFrom: {name: demo-registry}
    verificationPolicyFrom: {name: demo-verification-policy, key: policy.yaml}
    transport: {plainHTTP: true}
  policy:
    apply: OnApproval
    allowDestructive: false
    driftSeverity: all
  interval: 1m
YAML
kubectl ptah wait "schema/$SCHEMA" -n "$NAMESPACE" --for in-sync --timeout 10m >/dev/null ||
	fail_showing 'the artifact the database already matches did not converge without a plan'

say 'publishing the next revision, and pointing the resource at it with kubectl ptah set-source'
SECOND=$(push_schema schema-v2.sql v2)
kubectl ptah set-source "schema/$SCHEMA" -n "$NAMESPACE" --wait --for approval --wait-timeout 10m \
	--oci "$SECOND" >/dev/null ||
	fail_showing 'set-source did not bring the resource to wait for an approval of the next revision'
RESOLVED=$(kubectl -n "$NAMESPACE" get "ptahschema/$SCHEMA" -o jsonpath='{.status.source.digest}')
[ "$RESOLVED" = "${SECOND##*@}" ] ||
	fail "the resource resolved $RESOLVED, and set-source pointed it at $SECOND"
approve_current_plan "$NEXT_APPROVAL"

say 'reading the live database again'
kubectl -n "$NAMESPACE" exec deploy/demo-psql -- psql -c '\d customers' | grep -q signed_up_at ||
	fail 'the customers table has no signed_up_at column, so the next revision did not reach the database'

say "repeated the scenario with no script of ours: one account asked and could not create a Job, another approved and could not either ($JOBS of the operator's Jobs ran), then the same from an artifact ptah published and set-source moved"
