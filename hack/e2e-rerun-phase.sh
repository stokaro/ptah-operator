#!/bin/sh

set -eu

# Put one lifecycle phase back on the cluster a failed run left behind.
#
# A full run spends about an hour and three quarters building the state the
# last phase needs before that phase can fail again, which is the wrong loop
# for diagnosing the phase itself. E2E_KEEP_ON_FAILURE=1 already retains the
# cluster, the registry, the database and the work directory; hack/e2e-kind.sh
# records the exact environment it handed each phase beside them. This runs a
# phase from the working tree against that state, so an edit to a phase script
# or to the chart is measured in minutes.
#
# What it does not do: rebuild the manager image. The images the cluster pulls
# were built from the commit the run snapshotted, so a change under cmd/ or
# internal/ still needs a full run. It says so rather than pretending.
#
# A phase that creates state cannot simply run a second time. The migrations and
# reference-data phases each want a database nothing has touched, create their
# fixtures rather than apply them, and publish to version tags the registry will
# not move. So this sets E2E_PHASE_RERUN, and those two phases clear what the
# earlier run of themselves left behind and publish under a repository of their
# own before they start. The other phases are rerun as they are.
#
# A phase that never started has no recorded environment, so it cannot be rerun
# here: a run that failed in migrations recorded nothing for reference-data.
#
# Usage: hack/e2e-rerun-phase.sh <retained-work-dir> <phase>

unset CDPATH
ROOT_DIR=$(cd "$(dirname -- "$0")/.." && pwd)

fail() {
	printf 'e2e rerun: %s\n' "$*" >&2
	exit 1
}

[ "$#" -eq 2 ] || fail "usage: hack/e2e-rerun-phase.sh <retained-work-dir> <phase>"
RERUN_WORK_DIR=$1
RERUN_PHASE=$2

[ -d "$RERUN_WORK_DIR" ] || fail "retained work directory is missing: $RERUN_WORK_DIR"
RERUN_ENV_FILE=$RERUN_WORK_DIR/phase-$RERUN_PHASE.env
if [ ! -f "$RERUN_ENV_FILE" ] || [ -L "$RERUN_ENV_FILE" ]; then
	printf 'e2e rerun: %s has no recorded environment for phase %s\n' \
		"$RERUN_WORK_DIR" "$RERUN_PHASE" >&2
	printf 'e2e rerun: recorded phases:%s\n' \
		"$(for recorded in "$RERUN_WORK_DIR"/phase-*.env; do
			[ -f "$recorded" ] || continue
			recorded=${recorded##*/phase-}
			printf ' %s' "${recorded%.env}"
		done)" >&2
	exit 1
fi

case $RERUN_PHASE in
	upgrade | uninstall) RERUN_SCRIPT=hack/e2e-crd-upgrade.sh ;;
	ha) RERUN_SCRIPT=hack/e2e-ha.sh ;;
	assert) RERUN_SCRIPT=hack/e2e-assert.sh ;;
	cert-rotation) RERUN_SCRIPT=hack/e2e-cert-rotation.sh ;;
	dataplane) RERUN_SCRIPT=hack/e2e-dataplane.sh ;;
	migrations-postgresql | migrations-mysql) RERUN_SCRIPT=hack/e2e-migrations.sh ;;
	reference-data-postgresql | reference-data-mysql) RERUN_SCRIPT=hack/e2e-reference-data.sh ;;
	*) fail "unsupported phase $RERUN_PHASE" ;;
esac
[ -x "$ROOT_DIR/$RERUN_SCRIPT" ] || fail "phase script is not executable: $RERUN_SCRIPT"

# The recorded file is name=value, one per line, written by `env`. A value can
# hold anything, so each line is exported through the shell rather than sourced
# as code.
RERUN_KUBECONFIG=
while IFS= read -r recorded_line; do
	case $recorded_line in
		E2E_[A-Z0-9_]*=*) ;;
		*) fail "recorded environment holds a line that is not an E2E assignment" ;;
	esac
	recorded_name=${recorded_line%%=*}
	recorded_value=${recorded_line#*=}
	export "$recorded_name=$recorded_value"
	[ "$recorded_name" != E2E_KUBECONFIG ] || RERUN_KUBECONFIG=$recorded_value
done <"$RERUN_ENV_FILE"

[ -n "$RERUN_KUBECONFIG" ] || fail "recorded environment does not name a kubeconfig"
[ -f "$RERUN_KUBECONFIG" ] || fail "recorded kubeconfig is gone: $RERUN_KUBECONFIG"
kubectl --kubeconfig "$RERUN_KUBECONFIG" --request-timeout=15s version -o json >/dev/null 2>&1 ||
	fail "the retained cluster does not answer through $RERUN_KUBECONFIG"

# Unique per rerun, because the repository a rerun publishes under has to be
# new each time: a second rerun would meet the first rerun's tags.
E2E_PHASE_RERUN="r$(date +%s)"
export E2E_PHASE_RERUN

printf 'e2e rerun: %s against the cluster in %s, as rerun %s\n' \
	"$RERUN_PHASE" "$RERUN_WORK_DIR" "$E2E_PHASE_RERUN"
printf 'e2e rerun: the manager image is the one that run built; a change under cmd/ or internal/ needs a full run\n'
exec "$ROOT_DIR/$RERUN_SCRIPT"
