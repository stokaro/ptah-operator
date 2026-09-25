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
	lab_database_username=$(jq -er '.username' "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")
	lab_database_password=$(jq -er '.password' "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")
	lab_database_name=$(jq -er '.database' "$E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE")

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
	# The operator reads one key, `url`. The parts are there for libpq, which
	# has no variable for a connection string: a client that had to assemble
	# one would need a shell around every command a scenario shows.
	k -n "$E2E_TEST_NAMESPACE" create secret generic demo-database \
		--from-literal=url="$lab_database_url" \
		--from-literal=host="$E2E_EXTERNAL_POSTGRES_SERVICE" \
		--from-literal=port=5432 \
		--from-literal=username="$lab_database_username" \
		--from-literal=password="$lab_database_password" \
		--from-literal=database="$lab_database_name" \
		--dry-run=client -o yaml | k apply -f - >/dev/null

	# The default ServiceAccount carries the pull secret, so every Pod in the
	# namespace can pull from the isolated registry: the operator's executor
	# Jobs, and the Job that publishes a schema.
	k -n "$E2E_TEST_NAMESPACE" patch serviceaccount default --type=merge \
		--patch '{"imagePullSecrets":[{"name":"demo-registry-pull"}]}' >/dev/null

	# A psql client, running in the namespace and reading the same Secret the
	# operator reads. Standing the environment up is this script's job; what a
	# scenario then shows is `kubectl exec`, which is nobody's private
	# interface. libpq takes the connection out of the environment, so the
	# command a scenario prints is `psql -c "<statement>"` and nothing else.
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--arg image "$LAB_POSTGRES_IMAGE" '
    {
      apiVersion: "apps/v1", kind: "Deployment",
      metadata: {namespace: $namespace, name: "demo-psql"},
      spec: {
        replicas: 1,
        selector: {matchLabels: {"app.kubernetes.io/name": "demo-psql"}},
        template: {
          metadata: {labels: {"app.kubernetes.io/name": "demo-psql"}},
          spec: {
            automountServiceAccountToken: false,
            containers: [{
              name: "psql", image: $image, imagePullPolicy: "IfNotPresent",
              command: ["sleep", "infinity"],
              env: [
                {name: "PGHOST", valueFrom: {secretKeyRef: {name: "demo-database", key: "host"}}},
                {name: "PGPORT", valueFrom: {secretKeyRef: {name: "demo-database", key: "port"}}},
                {name: "PGUSER", valueFrom: {secretKeyRef: {name: "demo-database", key: "username"}}},
                {name: "PGPASSWORD", valueFrom: {secretKeyRef: {name: "demo-database", key: "password"}}},
                {name: "PGDATABASE", valueFrom: {secretKeyRef: {name: "demo-database", key: "database"}}}
              ]
            }]
          }
        }
      }
    }' | k apply -f - >/dev/null
	k -n "$E2E_TEST_NAMESPACE" rollout status deployment/demo-psql --timeout=180s >/dev/null

	lab_prepare_mysql

	# The policy ConfigMap is immutable, and the operator refuses one that is
	# not: a policy that could be edited after an artifact was verified against
	# it is a policy that decided nothing. Immutable also means it cannot be
	# applied over, so a changed policy is replaced rather than patched.
	lab_apply_policy demo-verification-policy verification-policy.yaml
	lab_apply_policy demo-migration-verification-policy migration-verification-policy.yaml
}

# lab_prepare_mysql stands up the MySQL the engine-specific scenarios run
# against: a server in the namespace, its credentials as a Secret the operator
# reads by the same `url` key, and a client that reads the same Secret.
#
# In the cluster rather than beside it, because nothing about a partially
# committed MySQL migration depends on where the server runs, and a server here
# is one Deployment instead of a second container the bootstrap would have to
# route to. The image is the one the bootstrap mirrored for the acceptance
# suite, so it is the MySQL version this repository supports.
lab_prepare_mysql() {
	lab_require LAB_MYSQL_IMAGE
	mkdir -p "$LAB_WORK"
	[ -s "$LAB_WORK/mysql-password" ] ||
		head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' >"$LAB_WORK/mysql-password"
	lab_mysql_password=$(cat "$LAB_WORK/mysql-password")
	lab_mysql_host="demo-mysql.${E2E_TEST_NAMESPACE}.svc.cluster.local"
	k -n "$E2E_TEST_NAMESPACE" create secret generic demo-mysql-database \
		--from-literal=url="mysql://demo:${lab_mysql_password}@tcp(${lab_mysql_host}:3306)/demo" \
		--from-literal=host="$lab_mysql_host" \
		--from-literal=username=demo \
		--from-literal=password="$lab_mysql_password" \
		--from-literal=database=demo \
		--dry-run=client -o yaml | k apply -f - >/dev/null
	# The mysql client takes its password from MYSQL_PWD and its user from
	# USER, so what a scenario prints is `mysql demo -e "<statement>"` with
	# nothing secret on the command line.
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--arg image "$LAB_MYSQL_IMAGE" '
    def secret($key): {valueFrom: {secretKeyRef: {name: "demo-mysql-database", key: $key}}};
    def labels($name): {"app.kubernetes.io/name": $name};
    {
      apiVersion: "v1", kind: "List",
      items: [
        {
          apiVersion: "apps/v1", kind: "Deployment",
          metadata: {namespace: $namespace, name: "demo-mysql"},
          spec: {
            replicas: 1,
            selector: {matchLabels: labels("demo-mysql")},
            template: {
              metadata: {labels: labels("demo-mysql")},
              spec: {
                automountServiceAccountToken: false,
                containers: [{
                  name: "mysql", image: $image, imagePullPolicy: "IfNotPresent",
                  ports: [{name: "mysql", containerPort: 3306}],
                  env: [
                    ({name: "MYSQL_ROOT_PASSWORD"} + secret("password")),
                    ({name: "MYSQL_PASSWORD"} + secret("password")),
                    {name: "MYSQL_USER", value: "demo"},
                    {name: "MYSQL_DATABASE", value: "demo"}
                  ],
                  readinessProbe: {
                    exec: {command: ["sh", "-c", "MYSQL_PWD=\"$MYSQL_PASSWORD\" mysqladmin -h 127.0.0.1 -u demo ping"]},
                    periodSeconds: 5
                  }
                }]
              }
            }
          }
        },
        {
          apiVersion: "v1", kind: "Service",
          metadata: {namespace: $namespace, name: "demo-mysql"},
          spec: {selector: labels("demo-mysql"), ports: [{name: "mysql", port: 3306, targetPort: "mysql"}]}
        },
        {
          apiVersion: "apps/v1", kind: "Deployment",
          metadata: {namespace: $namespace, name: "demo-mysql-client"},
          spec: {
            replicas: 1,
            selector: {matchLabels: labels("demo-mysql-client")},
            template: {
              metadata: {labels: labels("demo-mysql-client")},
              spec: {
                automountServiceAccountToken: false,
                containers: [{
                  name: "mysql", image: $image, imagePullPolicy: "IfNotPresent",
                  command: ["sleep", "infinity"],
                  env: [
                    ({name: "MYSQL_HOST"} + secret("host")),
                    ({name: "MYSQL_PWD"} + secret("password")),
                    ({name: "USER"} + secret("username"))
                  ]
                }]
              }
            }
          }
        }
      ]
    }' | k apply -f - >/dev/null
	k -n "$E2E_TEST_NAMESPACE" rollout status deployment/demo-mysql --timeout=300s >/dev/null
	k -n "$E2E_TEST_NAMESPACE" rollout status deployment/demo-mysql-client --timeout=180s >/dev/null
}

# lab_apply_policy writes one immutable verification policy ConfigMap.
#
# Two of them, because a migration directory and a declared schema are
# different artifact types and a policy that accepted both would let either
# stand in for the other at the same reference.
lab_apply_policy() {
	lab_policy_name=$1
	lab_policy_file=$2
	jq -n \
		--arg namespace "$E2E_TEST_NAMESPACE" \
		--arg name "$lab_policy_name" \
		--rawfile policy "$LAB_ROOT/demo/policy/$lab_policy_file" '
    {
      apiVersion: "v1", kind: "ConfigMap",
      metadata: {namespace: $namespace, name: $name},
      immutable: true,
      data: {"policy.yaml": $policy}
    }' >"$LAB_WORK/$lab_policy_name.json"
	if ! k apply -f "$LAB_WORK/$lab_policy_name.json" >/dev/null 2>&1; then
		k -n "$E2E_TEST_NAMESPACE" delete configmap "$lab_policy_name" \
			--ignore-not-found >/dev/null
		k apply -f "$LAB_WORK/$lab_policy_name.json" >/dev/null
	fi
}

# lab_credentials prints the registry credentials as environment assignments.
#
# The lab generates them, so it is the only thing that can say what they are.
# What it will not do is publish on the reader's behalf: `ptah schema push` is
# the product's own command, and a demonstration that wrapped it would be
# teaching an interface that exists nowhere else.
lab_credentials() {
	lab_require E2E_REGISTRY_CREDENTIALS_FILE
	printf 'PTAH_OCI_USERNAME=%s\n' "$(jq -er '.username' "$E2E_REGISTRY_CREDENTIALS_FILE")"
	printf 'PTAH_OCI_PASSWORD=%s\n' "$(jq -er '.password' "$E2E_REGISTRY_CREDENTIALS_FILE")"
	printf 'PTAH_OCI_REGISTRY=%s\n' "$(lab_registry_address)"
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
	# The migration kinds go with them. A PtahMigration left behind claims the
	# same database the next scenario's PtahSchema does, and the operator
	# refuses a database more than one resource claims -- so a leftover would
	# not corrupt the next scenario, it would block it.
	k -n "$E2E_TEST_NAMESPACE" delete ptahmigration --all --ignore-not-found \
		--timeout=120s >/dev/null
	k -n "$E2E_TEST_NAMESPACE" delete ptahmigrationapproval --all --ignore-not-found \
		--timeout=60s >/dev/null
	k -n "$E2E_TEST_NAMESPACE" delete ptahmigrationplan --all --ignore-not-found \
		--timeout=60s >/dev/null
	# Every scenario starts from an empty database, so a drop that did not run
	# is not a detail to carry on from: the next scenario would be recorded
	# against whatever the last one left, and the failure would surface as a
	# plan nobody can explain several steps later.
	# The revision table goes too. A migration history the next scenario did not
	# create is a history it would continue from, and the versioned workflow
	# reads that table before it reads anything else.
	k -n "$E2E_TEST_NAMESPACE" exec deploy/demo-psql -- \
		psql -qAt -c "DROP TABLE IF EXISTS orders, customers, shipments CASCADE;
		              DROP TABLE IF EXISTS countries, regions CASCADE;
		              DROP TABLE IF EXISTS schema_migrations CASCADE;
		              DROP SCHEMA IF EXISTS atlas_schema_revisions CASCADE" >/dev/null ||
		lab_fail "could not empty the demonstration database"
	k -n "$E2E_TEST_NAMESPACE" exec deploy/demo-mysql-client -- \
		mysql demo -e "DROP TABLE IF EXISTS deliveries, schema_migrations" >/dev/null ||
		lab_fail "could not empty the demonstration MySQL database"
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
	# Built once, here, so a scenario's steps are the commands and not the
	# preparation for them. A reader installs kubectl-ptah from a release and
	# has ptah already; the lab is standing in for both.
	lab_tools >/dev/null
}

# lab_release_tunnel closes the SSH forward the bootstrap left running.
#
# The sweep below removes containers and volumes by owner label, and the tunnel
# carries none: it is a process on this machine, not a Docker object, so nothing
# there can see it. The harness that started it would have killed it from its own
# cleanup, and the bootstrap released that cleanup on purpose, which leaves this
# as the only place that can.
#
# The port comes from the run id, so it is the same port every time: a forward
# left bound does not leak quietly, it refuses the next lab with `Address already
# in use` and says nothing about why.
#
# The recorded pid is checked before it is killed. A pid file outlives the
# process it names and the numbers are reused, so killing the number alone turns
# a leak into a way to end somebody else's work. The forward the harness wrote
# down is what tells this tunnel from whatever inherited its number.
#
# An environment written before the tunnel was recorded carries neither value,
# and a lab reached over direct host access has no tunnel at all. Both are
# silent: there is nothing to release, which is not a failure to release it.
lab_release_tunnel() {
	[ -n "${E2E_TUNNEL_PID:-}" ] || return 0
	if ! kill -0 "$E2E_TUNNEL_PID" 2>/dev/null; then
		return 0
	fi
	lab_tunnel_command=$(ps -o command= -p "$E2E_TUNNEL_PID" 2>/dev/null || true)
	case "$lab_tunnel_command" in
	*ssh*"${E2E_TUNNEL_FORWARD:-__absent__}"*) ;;
	*)
		printf 'lab: process %s no longer forwards %s; leaving it alone\n' \
			"$E2E_TUNNEL_PID" "${E2E_TUNNEL_FORWARD:-<unrecorded>}" >&2
		lab_down_incomplete=1
		return 0
		;;
	esac
	kill "$E2E_TUNNEL_PID" 2>/dev/null || true
	printf 'lab: released the API tunnel on %s\n' "$E2E_TUNNEL_FORWARD"
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
	lab_release_tunnel
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
	lab_down_owner="operator.ptah.run/e2e-owner=$E2E_KIND_CLUSTER_NAME"
	lab_down_remove_labelled container
	lab_down_remove_labelled volume
	lab_down_remove_images

	if [ "$lab_down_incomplete" -ne 0 ]; then
		printf 'lab: teardown is incomplete; the environment file and the work directory are kept so it can be retried\n' >&2
		return 1
	fi
	# Only now. The work directory holds the task-scoped Docker context this
	# function resolves, so removing it before the teardown has finished leaves
	# a retry unable to reach the daemon that still holds what is left.
	lab_down_remove_work_directory
	lab_down_remove_lab_directory
	printf 'lab: removed\n'
}

# lab_down_list_labelled prints the ids of one kind of object this lab labelled.
lab_down_list_labelled() {
	case "$1" in
	container)
		docker --context "$E2E_DOCKER_CONTEXT" container ls \
			--all --quiet --filter "label=$lab_down_owner"
		;;
	volume)
		docker --context "$E2E_DOCKER_CONTEXT" volume ls \
			--quiet --filter "label=$lab_down_owner"
		;;
	esac
}

# lab_down_remove_labelled removes them, and says so when it could not.
#
# Every step is read. A Docker command that could not run at all lists nothing,
# and an unchecked list of nothing reads exactly like a lab with nothing left --
# which is how a teardown reports success over the task claim that refuses the
# next run.
lab_down_remove_labelled() {
	lab_down_kind=$1
	if ! lab_down_ids=$(lab_down_list_labelled "$lab_down_kind" 2>/dev/null); then
		printf 'lab: could not ask which %ss this lab created\n' "$lab_down_kind" >&2
		lab_down_incomplete=1
		return 0
	fi
	for lab_down_id in $lab_down_ids; do
		lab_down_remove_one "$lab_down_kind" "$lab_down_id" || {
			printf 'lab: could not remove %s %s\n' "$lab_down_kind" "$lab_down_id" >&2
			lab_down_incomplete=1
		}
	done
	# Asked again, for the reason the cluster is asked again: a removal that
	# reported nothing and a removal that did nothing are different answers.
	if ! lab_down_left=$(lab_down_list_labelled "$lab_down_kind" 2>/dev/null); then
		printf 'lab: could not ask whether any %s of this lab is left\n' "$lab_down_kind" >&2
		lab_down_incomplete=1
		return 0
	fi
	if [ -n "$lab_down_left" ]; then
		printf 'lab: %s left behind: %s\n' "$lab_down_kind" \
			"$(printf '%s' "$lab_down_left" | tr '\n' ' ')" >&2
		lab_down_incomplete=1
	fi
}

lab_down_remove_one() {
	case "$1" in
	container) docker --context "$E2E_DOCKER_CONTEXT" container rm -fv "$2" >/dev/null 2>&1 ;;
	volume) docker --context "$E2E_DOCKER_CONTEXT" volume rm "$2" >/dev/null 2>&1 ;;
	esac
}

# lab_down_remove_images removes what the bootstrap built and mirrored.
#
# They carry no owner label and their tags are shared with no other run, so the
# list it wrote is the only answer; a name pattern would reach another run's
# images. An image that is already gone is not a failure, and a daemon that
# cannot answer has already been reported by the labelled removals above.
lab_down_remove_images() {
	# The bootstrap writes the list with commas, because the caller sources
	# that file and a value with a space in it is a command line. Splitting on
	# them is this one expansion; everything after it is quoted.
	IFS=,
	# shellcheck disable=SC2086 # The comma-separated list is split on purpose.
	set -- ${E2E_CREATED_IMAGE_REFS:-}
	unset IFS
	for lab_down_image in "$@"; do
		docker --context "$E2E_DOCKER_CONTEXT" image inspect \
			"$lab_down_image" >/dev/null 2>&1 || continue
		docker --context "$E2E_DOCKER_CONTEXT" image rm \
			"$lab_down_image" >/dev/null 2>&1 || {
			printf 'lab: could not remove image %s\n' "$lab_down_image" >&2
			lab_down_incomplete=1
		}
	done
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

# lab_ptah puts the product's own CLI on PATH.
#
# Built from the exact Ptah source the bootstrap extracted for the executor
# image, so the binary that publishes an artifact is the build that will read it
# back. A reader with their own `ptah` installed does not need this: what the
# scenarios publish is `ptah schema push`, which is the same command either way.
#
# The executor image cannot supply it. That image is built for the cluster's
# platform, and a demonstration is watched from a machine that is often another
# one.
# lab_tools builds the two command-line tools a scenario uses and prints the
# directory holding them.
#
# `ptah` publishes a schema as an artifact and comes from the exact Ptah source
# the lab's executor was built from, so the CLI doing the publishing is the
# build that reads it back. `kubectl-ptah` reads a stored plan and comes from
# this repository, which is where it ships from.
lab_tools() {
	lab_tools_bin="$LAB_ROOT/demo/.lab/bin"
	mkdir -p "$lab_tools_bin"
	lab_tools_client
	lab_ptah
	printf '%s\n' "$lab_tools_bin"
}

# lab_tools_client builds the kubectl plugin from this repository.
lab_tools_client() {
	[ -x "$lab_tools_bin/kubectl-ptah" ] && return 0
	printf 'lab: building kubectl-ptah for this machine\n' >&2
	( cd "$LAB_ROOT" && go build -o "$lab_tools_bin/kubectl-ptah" ./cmd/kubectl-ptah ) ||
		lab_fail "could not build kubectl-ptah from $LAB_ROOT"
}

lab_ptah() {
	lab_ptah_bin="$LAB_ROOT/demo/.lab/bin"
	if [ -x "$lab_ptah_bin/ptah" ]; then
		return 0
	fi
	# A lab built from a prebuilt executor image archived no Ptah source, so
	# there is nothing here to build a CLI from. That is a configuration a
	# reader chose, not a fault: say what it costs and where a CLI comes from.
	[ -n "${E2E_PTAH_BUILD_CONTEXT:-}" ] ||
		lab_fail "this lab was built from the executor image ${E2E_EXECUTOR_IMAGE:-} rather than from Ptah source, so it kept none to build the CLI from; install ptah yourself and put it on PATH"
	[ -f "$E2E_PTAH_BUILD_CONTEXT/go.mod" ] ||
		lab_fail "the lab kept no Ptah source at $E2E_PTAH_BUILD_CONTEXT"
	mkdir -p "$lab_ptah_bin"
	printf 'lab: building ptah %s for this machine\n' "${E2E_PTAH_VERSION:-}" >&2
	( cd "$E2E_PTAH_BUILD_CONTEXT" && go build -o "$lab_ptah_bin/ptah" ./cmd/ptah ) ||
		lab_fail "could not build the Ptah CLI from $E2E_PTAH_BUILD_CONTEXT"
}

# lab_registry_address prints the address a client outside the cluster publishes
# to, opening a tunnel first when the Docker daemon is somewhere else.
#
# The registry is one registry with two names. Inside the cluster it is a
# Service; from a publishing client it is a port on the Docker host. That is
# what a registry looks like in an ordinary deployment, and the digest a push
# returns is the digest the operator resolves.
lab_registry_address() {
	lab_require E2E_REGISTRY_HOST_ADDRESS
	# Ask before building. The address already answers when Docker is the
	# reader's own, and when a tunnel from an earlier command is still up;
	# opening a second one would fail on the port the first is holding, which
	# reads as "the registry is unreachable" and is the opposite.
	if ! lab_registry_answers; then
		case "${E2E_DOCKER_ENDPOINT:-}" in
		ssh://*) lab_registry_tunnel ;;
		*) lab_fail "the registry does not answer at $E2E_REGISTRY_HOST_ADDRESS" ;;
		esac
		lab_registry_answers ||
			lab_fail "the registry still does not answer at $E2E_REGISTRY_HOST_ADDRESS"
	fi
	printf '%s\n' "$E2E_REGISTRY_HOST_ADDRESS"
}

# lab_registry_answers reports whether something is serving the registry API
# there. A 401 is an answer: the registry wants credentials, which is what a
# registry with credentials does.
lab_registry_answers() {
	lab_registry_code=$(curl -sS -o /dev/null -m 5 -w '%{http_code}' \
		"http://${E2E_REGISTRY_HOST_ADDRESS}/v2/" 2>/dev/null) || return 1
	case "$lab_registry_code" in
	200 | 401) return 0 ;;
	*) return 1 ;;
	esac
}

# lab_registry_tunnel forwards the registry's port from a remote Docker host.
#
# The harness opens one for its own run and closes it on exit, so a retained lab
# has none. This one is the lab's, recorded by process id, and `lab down` closes
# it; a reader whose Docker is local never reaches this path.
lab_registry_tunnel() {
	lab_tunnel_pidfile="$LAB_ROOT/demo/.lab/registry-tunnel.pid"
	if [ -f "$lab_tunnel_pidfile" ] && kill -0 "$(cat "$lab_tunnel_pidfile")" 2>/dev/null; then
		return 0
	fi
	lab_tunnel_port=${E2E_REGISTRY_HOST_ADDRESS##*:}
	lab_tunnel_host=${E2E_DOCKER_ENDPOINT#ssh://}
	mkdir -p "$(dirname "$lab_tunnel_pidfile")"
	ssh -f -N -o ExitOnForwardFailure=yes \
		-L "127.0.0.1:${lab_tunnel_port}:127.0.0.1:${lab_tunnel_port}" \
		"$lab_tunnel_host" ||
		lab_fail "could not forward the registry port from $lab_tunnel_host"
	pgrep -f "L 127.0.0.1:${lab_tunnel_port}:127.0.0.1:${lab_tunnel_port}" | head -1 >"$lab_tunnel_pidfile"
}

lab_fail() {
	printf 'lab: %s\n' "$1" >&2
	exit 1
}
