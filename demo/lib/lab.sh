#!/bin/sh
# The namespace the demonstration runs in, built on the bootstrapped cluster.
#
# The cluster, the chart, the digest-pinned images, the isolated registry, the
# external database and the admission policies all come from the end-to-end
# bootstrap, which is the one place they are declared. What is here is only what
# a namespace needs to reach them: the two Services that give Pods a route to
# the two Docker containers, the credentials as Secrets, and the verification
# policy. They live here because the scenarios shape them -- a customers and
# orders domain, and a policy a reader can read in full.

# lab_kubectl runs kubectl against the lab.
k() {
	kubectl --kubeconfig "$E2E_KUBECONFIG" "$@"
}

lab_require() {
	for lab_name in "$@"; do
		eval "lab_value=\${$lab_name:-}"
		[ -n "$lab_value" ] || {
			printf 'lab: %s is not set; bring the lab up with make demo-up\n' "$lab_name" >&2
			exit 1
		}
	done
}

# lab_prepare creates the namespace and everything in it that a scenario needs.
#
# It is idempotent: every object is applied, so running it twice is running it
# once. A scenario's own reset removes what that scenario made, and this
# function is what a fresh lab starts from.
lab_prepare() {
	LAB_WORK=${LAB_WORK:-$LAB_ROOT/demo/.lab}
	mkdir -p "$LAB_WORK"
	lab_require E2E_KUBECONFIG E2E_TEST_NAMESPACE E2E_REGISTRY_SERVICE E2E_REGISTRY_IP \
		E2E_REGISTRY_HOST E2E_REGISTRY_USERNAME E2E_REGISTRY_CREDENTIALS_FILE \
		E2E_EXTERNAL_POSTGRES_SERVICE E2E_EXTERNAL_POSTGRES_IP \
		E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE

	k create namespace "$E2E_TEST_NAMESPACE" --dry-run=client -o yaml | k apply -f - >/dev/null

	# Both routes are a Service with an EndpointSlice written by hand. The
	# registry and the database are Docker containers on the kind network, so
	# there is no selector that could find them, and a Pod resolving a
	# .svc.cluster.local name is answered by cluster DNS rather than by the
	# node's resolver.
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--arg registry "$E2E_REGISTRY_SERVICE" \
		--arg registryAddress "$E2E_REGISTRY_IP" \
		--arg database "$E2E_EXTERNAL_POSTGRES_SERVICE" \
		--arg databaseAddress "$E2E_EXTERNAL_POSTGRES_IP" '
    def route($name; $address; $port):
      [
        {
          apiVersion: "v1", kind: "Service",
          metadata: {namespace: $namespace, name: $name},
          spec: {ports: [{name: "tcp", port: $port, protocol: "TCP", targetPort: $port}]}
        },
        {
          apiVersion: "discovery.k8s.io/v1", kind: "EndpointSlice",
          metadata: {
            namespace: $namespace, name: ($name + "-docker"),
            labels: {"kubernetes.io/service-name": $name}
          },
          addressType: "IPv4",
          endpoints: [{addresses: [$address]}],
          ports: [{name: "tcp", port: $port, protocol: "TCP"}]
        }
      ];
    {
      apiVersion: "v1", kind: "List",
      items: (route($registry; $registryAddress; 5000) + route($database; $databaseAddress; 5432))
    }' | k apply -f - >/dev/null

	lab_registry_username=$(jq -er '.username' "$E2E_REGISTRY_CREDENTIALS_FILE")
	lab_registry_password=$(jq -er '.password' "$E2E_REGISTRY_CREDENTIALS_FILE")
	lab_database_url=$(jq -er '.url' "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")

	# The credentials reach the cluster as Secrets and never as an argument: a
	# process list is readable, and this operator exists to keep a database
	# password out of the places a schema change would otherwise carry it.
	k -n "$E2E_TEST_NAMESPACE" create secret generic demo-registry \
		--from-literal=username="$lab_registry_username" \
		--from-literal=password="$lab_registry_password" \
		--from-literal=registry="$E2E_REGISTRY_HOST" \
		--from-literal=allowPlainHTTP=true \
		--dry-run=client -o yaml | k apply -f - >/dev/null
	k -n "$E2E_TEST_NAMESPACE" create secret docker-registry demo-registry-pull \
		--docker-server="$E2E_REGISTRY_HOST" \
		--docker-username="$lab_registry_username" \
		--docker-password="$lab_registry_password" \
		--dry-run=client -o yaml | k apply -f - >/dev/null
	k -n "$E2E_TEST_NAMESPACE" create secret generic demo-database \
		--from-literal=url="$lab_database_url" \
		--dry-run=client -o yaml | k apply -f - >/dev/null

	# The default ServiceAccount carries the pull secret, so every Pod in the
	# namespace can pull from the isolated registry: the operator's executor
	# Jobs, and the Job that publishes a schema.
	k -n "$E2E_TEST_NAMESPACE" patch serviceaccount default --type=merge \
		--patch '{"imagePullSecrets":[{"name":"demo-registry-pull"}]}' >/dev/null

	# The policy ConfigMap is immutable, and the operator refuses one that is
	# not: a policy that could be edited after an artifact was verified against
	# it is a policy that decided nothing. Immutable also means it cannot be
	# applied over, so a changed policy is replaced rather than patched.
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--rawfile policy "$LAB_ROOT/demo/policy/verification-policy.yaml" '
    {
      apiVersion: "v1", kind: "ConfigMap",
      metadata: {namespace: $namespace, name: "demo-verification-policy"},
      immutable: true,
      data: {"policy.yaml": $policy}
    }' >"$LAB_WORK/verification-policy.json"
	if ! k apply -f "$LAB_WORK/verification-policy.json" >/dev/null 2>&1; then
		k -n "$E2E_TEST_NAMESPACE" delete configmap demo-verification-policy \
			--ignore-not-found >/dev/null
		k apply -f "$LAB_WORK/verification-policy.json" >/dev/null
	fi
}

# lab_publish pushes one schema revision to the isolated registry and prints the
# digest it was stored under.
#
# The push is a `ptah schema push`, and it runs as a Job because the registry
# lives inside the cluster. The executor image is the one the chart pins, so
# what publishes the artifact is the Ptah build that will read it back.
lab_publish() {
	lab_publish_revision=$1
	lab_publish_repository=${2:-demo}
	lab_publish_file="$LAB_ROOT/demo/schemas/${lab_publish_revision}.sql"
	[ -f "$lab_publish_file" ] || {
		printf 'lab: no schema fixture at %s\n' "$lab_publish_file" >&2
		exit 1
	}
	lab_require E2E_EXECUTOR_IMAGE E2E_REGISTRY_HOST
	lab_publish_job="demo-push-${lab_publish_revision}"
	# The tag carries a nonce, and the push declares no artifact version.
	# A Ptah artifact is not byte-identical between two pushes of one file, and
	# both a tag and a declared version are write-once, so republishing a
	# revision would be refused for disagreeing with itself. What the operator
	# is pointed at is the digest the push returns, and the tag is only how
	# this push is addressed while it happens.
	lab_publish_nonce=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
	lab_publish_reference="oci://${E2E_REGISTRY_HOST}/schemas/${lab_publish_repository}:${lab_publish_revision}-${lab_publish_nonce}"

	k -n "$E2E_TEST_NAMESPACE" delete job "$lab_publish_job" --ignore-not-found >/dev/null
	k -n "$E2E_TEST_NAMESPACE" create configmap "$lab_publish_job" \
		--from-file=schema.sql="$lab_publish_file" \
		--dry-run=client -o yaml | k apply -f - >/dev/null
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--arg name "$lab_publish_job" \
		--arg image "$E2E_EXECUTOR_IMAGE" \
		--arg reference "$lab_publish_reference" \
		--arg version "$lab_publish_revision" '
    {
      apiVersion: "batch/v1", kind: "Job",
      metadata: {
        namespace: $namespace, name: $name,
        labels: {"app.kubernetes.io/component": "demo-publisher"}
      },
      spec: {
        backoffLimit: 0, activeDeadlineSeconds: 300,
        template: {
          spec: {
            restartPolicy: "Never", automountServiceAccountToken: false,
            securityContext: {
              runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532,
              seccompProfile: {type: "RuntimeDefault"}
            },
            containers: [{
              name: "publisher", image: $image, imagePullPolicy: "IfNotPresent",
              command: ["/usr/local/bin/ptah"],
              args: [
                "schema", "push", $reference, "--schema-file", "/schema/schema.sql",
                "--dialect", "postgres", "--plain-http"
              ],
              env: [
                {name: "HOME", value: "/work"},
                {name: "TMPDIR", value: "/work"},
                {name: "PTAH_OCI_USERNAME", valueFrom: {secretKeyRef: {name: "demo-registry", key: "username"}}},
                {name: "PTAH_OCI_PASSWORD", valueFrom: {secretKeyRef: {name: "demo-registry", key: "password"}}},
                {name: "PTAH_OCI_REGISTRY", valueFrom: {secretKeyRef: {name: "demo-registry", key: "registry"}}}
              ],
              securityContext: {
                allowPrivilegeEscalation: false, readOnlyRootFilesystem: true,
                capabilities: {drop: ["ALL"]}
              },
              volumeMounts: [
                {name: "schema", mountPath: "/schema", readOnly: true},
                {name: "work", mountPath: "/work"}
              ]
            }],
            volumes: [
              {name: "schema", configMap: {name: $name}},
              {name: "work", emptyDir: {sizeLimit: "64Mi"}}
            ]
          }
        }
      }
    }' | k apply -f - >/dev/null
	# Wait for the Job to finish either way. Waiting only for Complete turns a
	# failed push into a five-minute timeout, and the reason it failed is in
	# the logs the whole time.
	lab_publish_deadline=$(($(date +%s) + 300))
	while :; do
		lab_publish_state=$(k -n "$E2E_TEST_NAMESPACE" get job "$lab_publish_job" \
			-o jsonpath='{.status.succeeded}/{.status.failed}')
		case "$lab_publish_state" in
		1/*) break ;;
		*/[1-9]*)
			printf 'lab: publishing %s failed\n' "$lab_publish_revision" >&2
			k -n "$E2E_TEST_NAMESPACE" logs "job/$lab_publish_job" >&2
			return 1
			;;
		esac
		[ "$(date +%s)" -lt "$lab_publish_deadline" ] || {
			printf 'lab: publishing %s did not finish within 300s\n' "$lab_publish_revision" >&2
			return 1
		}
		sleep 2
	done
	# The digest comes out of what the push printed, not out of a second
	# computation over the file: what the operator will pull is what the
	# registry stored, and the push is the only thing that saw both.
	k -n "$E2E_TEST_NAMESPACE" logs "job/$lab_publish_job" |
		sed -n 's/.*\(sha256:[0-9a-f]\{64\}\).*/\1/p' | head -1
	# The Job stays: a scenario shows the `ptah schema push` it ran, and the
	# Job is where that command is written down. The ConfigMap holding the
	# schema file has done its work.
	k -n "$E2E_TEST_NAMESPACE" delete configmap "$lab_publish_job" --ignore-not-found >/dev/null
}

# lab_psql runs one statement against the demonstration's database.
#
# It runs in the cluster, as a Pod reading the same Secret the operator reads,
# so a scenario that shows the database never puts a URL on a command line.
#
# Both of psql's streams arrive as one. A terminal shows them interleaved, and
# psql writes some of what a reader came for -- "Did not find any relations" --
# to stderr; splitting them here would publish half of what the command said.
lab_psql() {
	lab_psql_statement=$1
	lab_require E2E_TEST_NAMESPACE LAB_POSTGRES_IMAGE
	lab_psql_pod=demo-psql
	k -n "$E2E_TEST_NAMESPACE" delete pod "$lab_psql_pod" --ignore-not-found --wait >/dev/null
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--arg name "$lab_psql_pod" \
		--arg image "$LAB_POSTGRES_IMAGE" \
		--arg statement "$lab_psql_statement" '
    {
      apiVersion: "v1", kind: "Pod",
      metadata: {namespace: $namespace, name: $name},
      spec: {
        restartPolicy: "Never", automountServiceAccountToken: false,
        containers: [{
          name: "psql", image: $image, imagePullPolicy: "IfNotPresent",
          command: ["sh", "-c", "exec psql \"$DATABASE_URL\" --no-psqlrc --pset pager=off -v ON_ERROR_STOP=1 -c \"$STATEMENT\" 2>&1"],
          env: [
            {name: "DATABASE_URL", valueFrom: {secretKeyRef: {name: "demo-database", key: "url"}}},
            {name: "STATEMENT", value: $statement}
          ]
        }]
      }
    }' | k apply -f - >/dev/null
	k -n "$E2E_TEST_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded \
		--timeout=120s "pod/$lab_psql_pod" >/dev/null 2>&1 || true
	k -n "$E2E_TEST_NAMESPACE" logs "$lab_psql_pod"
	lab_psql_phase=$(k -n "$E2E_TEST_NAMESPACE" get pod "$lab_psql_pod" -o jsonpath='{.status.phase}')
	k -n "$E2E_TEST_NAMESPACE" delete pod "$lab_psql_pod" --ignore-not-found --wait=false >/dev/null
	[ "$lab_psql_phase" = Succeeded ] || return 1
}

# lab_reset returns the namespace and the database to the state every scenario
# starts from: no schema resource, no plan, no approval, and an empty database.
#
# It is not part of a recording. A reader is watching the scenario, and the
# preparation is what makes the scenario reproducible rather than what it shows.
lab_reset() {
	lab_require E2E_TEST_NAMESPACE
	k -n "$E2E_TEST_NAMESPACE" delete ptahschema --all --ignore-not-found \
		--timeout=120s >/dev/null
	k -n "$E2E_TEST_NAMESPACE" delete ptahschemaapproval --all --ignore-not-found \
		--timeout=60s >/dev/null
	k -n "$E2E_TEST_NAMESPACE" delete ptahschemaplan --all --ignore-not-found \
		--timeout=60s >/dev/null
	k -n "$E2E_TEST_NAMESPACE" delete job -l app.kubernetes.io/component=demo-publisher \
		--ignore-not-found --timeout=60s >/dev/null
	lab_psql "DROP TABLE IF EXISTS orders, customers CASCADE" >/dev/null
}

# lab_manifest renders one manifest template.
#
# The template is the file a reader reads; what is substituted is only what
# cannot be written down in advance -- the namespace this lab happened to get,
# the registry it runs, and the digest a push returned. The policy fields are
# substituted too, because the scenarios differ in exactly those and a second
# near-identical manifest is how two of them drift apart.
#
# What it will not do is choose a policy. A renderer that defaulted `apply` to
# something other than the API's default would be deciding, quietly, the one
# thing a reader of the rendered manifest most needs to have decided
# themselves -- and the manifest it printed would not be the manifest the API
# would have produced from the same intent.
lab_manifest() {
	[ -n "${APPLY:-}" ] || {
		printf 'lab: set APPLY to the policy this scenario is about (Never, OnApproval or Always)\n' >&2
		exit 1
	}
	case "${APPLY}" in
	Never | OnApproval | Always) ;;
	*)
		printf 'lab: APPLY=%s is not a policy the API accepts\n' "$APPLY" >&2
		exit 1
		;;
	esac
	lab_manifest_name=$1
	lab_manifest_digest=$2
	lab_manifest_file="$LAB_ROOT/demo/manifests/${lab_manifest_name}.yaml"
	[ -f "$lab_manifest_file" ] || {
		printf 'lab: no manifest template at %s\n' "$lab_manifest_file" >&2
		exit 1
	}
	lab_require E2E_TEST_NAMESPACE E2E_REGISTRY_HOST
	sed \
		-e "s|\${NAMESPACE}|$E2E_TEST_NAMESPACE|g" \
		-e "s|\${REGISTRY}|$E2E_REGISTRY_HOST|g" \
		-e "s|\${DIGEST}|$lab_manifest_digest|g" \
		-e "s|\${APPLY}|${APPLY}|g" \
		-e "s|\${ALLOW_DESTRUCTIVE}|${ALLOW_DESTRUCTIVE:-false}|g" \
		-e "s|\${INTERVAL}|${INTERVAL:-1m}|g" \
		-e "s|\${SUSPEND}|${SUSPEND:-false}|g" \
		"$lab_manifest_file"
}

# lab_up builds the lab and prepares it.
#
# The build is the end-to-end harness stopped after its bootstrap, and it is
# skipped when a lab is already up. The namespace fixtures are applied either
# way: they are idempotent, and a lab brought up before a fixture changed would
# otherwise keep the old one.
lab_up() {
	mkdir -p "$(dirname "$LAB_ENVIRONMENT")"
	if [ -f "$LAB_ENVIRONMENT" ]; then
		printf 'lab: a lab is already up (%s); remove it with make demo-down\n' "$LAB_ENVIRONMENT"
	else
		K8S_VERSION="${LAB_KUBERNETES_VERSION:-$(support_newest_version)}" \
			E2E_STOP_AFTER=bootstrap \
			E2E_ENVIRONMENT_FILE="$LAB_ENVIRONMENT" \
			E2E_RUN_ID="${LAB_RUN_ID:-demo}" \
			DOCKER_CONTEXT="${DOCKER_CONTEXT:-remote-dev-container}" \
			"$LAB_ROOT/hack/e2e-kind.sh"
	fi
	read_environment
	lab_prepare
}

# lab_down removes what the bootstrap created, and nothing else.
#
# The bootstrap releases the harness's cleanup trap so the environment survives,
# which means this is the only thing that will ever remove it. Everything the
# harness creates carries an owner label naming this cluster, so containers and
# volumes are swept by that label rather than by a list of identifiers a later
# harness change would leave behind. The images and the work directory are not
# labelled, so the bootstrap writes down what it made.
#
# A lab left half-removed is worse than one left up: the task claim alone makes
# the next `make demo-up` refuse to run, and nothing says why.
lab_down() {
	[ -f "$LAB_ENVIRONMENT" ] || {
		printf 'lab: no lab to remove\n'
		return 0
	}
	read_environment
	lab_require E2E_KIND_CLUSTER_NAME E2E_DOCKER_CONTEXT

	lab_down_incomplete=0
	printf 'lab: removing cluster %s\n' "$E2E_KIND_CLUSTER_NAME"
	kind delete cluster --name "$E2E_KIND_CLUSTER_NAME" >/dev/null 2>&1 || true
	# Asked again, because the delete is quiet about a daemon it could not
	# reach. A teardown that says it removed a cluster still running is worse
	# than one that fails: the next run is refused and nothing says why.
	#
	# Three answers, not two. A daemon that did not answer is not a cluster
	# that is gone, and reading the first as the second is how the silence
	# became a success in the first place.
	if lab_down_clusters=$(kind get clusters 2>/dev/null); then
		if printf '%s\n' "$lab_down_clusters" | grep -qxF "$E2E_KIND_CLUSTER_NAME"; then
			printf 'lab: could not remove cluster %s\n' "$E2E_KIND_CLUSTER_NAME" >&2
			lab_down_incomplete=1
		fi
	else
		printf 'lab: could not ask whether cluster %s is gone\n' "$E2E_KIND_CLUSTER_NAME" >&2
		lab_down_incomplete=1
	fi

	# By owner label, so a container or volume this run created is removed and
	# one another run created is not. The label is the harness's, and it is on
	# every resource the harness makes outside the cluster.
	lab_down_owner="operator.ptah.dev/e2e-owner=$E2E_KIND_CLUSTER_NAME"
	for lab_down_container in $(docker --context "$E2E_DOCKER_CONTEXT" container ls \
		--all --quiet --filter "label=$lab_down_owner" 2>/dev/null); do
		docker --context "$E2E_DOCKER_CONTEXT" container rm -fv \
			"$lab_down_container" >/dev/null 2>&1 || true
	done
	for lab_down_volume in $(docker --context "$E2E_DOCKER_CONTEXT" volume ls \
		--quiet --filter "label=$lab_down_owner" 2>/dev/null); do
		docker --context "$E2E_DOCKER_CONTEXT" volume rm \
			"$lab_down_volume" >/dev/null 2>&1 || true
	done

	# The images the bootstrap built and mirrored. They carry no owner label and
	# their tags are shared with no other run, so the list it wrote is the only
	# answer; a name pattern would reach another run's images.
	for lab_down_image in ${E2E_CREATED_IMAGE_REFS:-}; do
		docker --context "$E2E_DOCKER_CONTEXT" image rm \
			"$lab_down_image" >/dev/null 2>&1 || true
	done

	lab_down_remove_work_directory
	if [ "$lab_down_incomplete" -ne 0 ]; then
		printf 'lab: teardown is incomplete; the environment file is kept so it can be retried\n' >&2
		return 1
	fi
	lab_down_remove_lab_directory
	printf 'lab: removed\n'
}

# lab_down_remove_work_directory removes the kubeconfig, the chart package and
# the credential files the bootstrap wrote.
#
# Only under the name the harness gives it. It holds a registry password and a
# database URL, so leaving it behind leaves those on disk; removing something
# else because a variable was pointed elsewhere is the worse mistake, so the
# path has to look like the harness's own.
lab_down_remove_work_directory() {
	case "${E2E_WORK_DIR:-}" in
	"${TMPDIR:-/tmp}"/ptah-operator-e2e.* | /tmp/ptah-operator-e2e.*)
		rm -rf -- "$E2E_WORK_DIR"
		;;
	'') ;;
	*)
		printf 'lab: refusing to remove unexpected work directory %s\n' \
			"$E2E_WORK_DIR" >&2
		;;
	esac
}

# lab_down_remove_lab_directory removes demo/.lab, and only demo/.lab.
#
# LAB_ENVIRONMENT is an input. Removing the directory it happens to sit in would
# delete whatever else is there, so the directory is removed only when it is the
# repository's own; otherwise the file that says a lab exists is removed and the
# rest is left alone.
lab_down_remove_lab_directory() {
	lab_down_directory=$(CDPATH='' cd -- "$(dirname -- "$LAB_ENVIRONMENT")" && pwd)
	if [ "$lab_down_directory" = "$LAB_ROOT/demo/.lab" ]; then
		rm -rf -- "$lab_down_directory"
		return 0
	fi
	printf 'lab: %s is not the lab directory, so only the environment file is removed\n' \
		"$lab_down_directory" >&2
	rm -f -- "$LAB_ENVIRONMENT"
}
