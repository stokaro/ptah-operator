#!/bin/sh
set -eu
cd /private/tmp/ptah-operator-020-profile
set -a
. /private/tmp/ptah-result-136.env
set +a
export LAB_ENVIRONMENT=/private/tmp/ptah-result-136.env
export LAB_WORK=/private/tmp/ptah-result-136-lab
export DOCKER_CONFIG="$E2E_DOCKER_CONFIG"
k() { kubectl --kubeconfig "$E2E_KUBECONFIG" "$@"; }
python3 /private/tmp/ptah-result-136-boundaries-20261003/default-logs.py
printf 'Enabling durable result delivery on %s\n' "$E2E_KIND_CLUSTER_NAME"
helm --kubeconfig "$E2E_KUBECONFIG" upgrade "$E2E_HELM_RELEASE" "$E2E_CHART_PACKAGE" --namespace "$E2E_OPERATOR_NAMESPACE" --wait --timeout 10m --values "$E2E_CANDIDATE_VALUES_FILE" --set resultDelivery.enabled=true
k -n "$E2E_OPERATOR_NAMESPACE" rollout status "deployment/$E2E_CONTROLLER_NAME" --timeout=180s
LAB_POSTGRES_IMAGE="$E2E_POSTGRES_IMAGE" LAB_MYSQL_IMAGE="$E2E_MYSQL_IMAGE" demo/bin/lab prepare
k -n "$E2E_TEST_NAMESPACE" create configmap result-schema --from-file=schema.sql=demo/schemas/v1.sql
python3 - <<'PY' | k create -f -
import os,json
namespace=os.environ['E2E_TEST_NAMESPACE']
reference='oci://'+os.environ['E2E_REGISTRY_HOST']+'/schemas/demo:result-test'
def env(name,key):return {'name':name,'valueFrom':{'secretKeyRef':{'name':'demo-registry','key':key}}}
container={'name':'publisher','image':os.environ['E2E_EXECUTOR_IMAGE'],'command':['/usr/local/bin/ptah'],'args':['schema','push',reference,'--schema-file','/schema/schema.sql','--dialect','postgres','--plain-http'],'env':[{'name':'HOME','value':'/work'},{'name':'TMPDIR','value':'/work'},env('PTAH_OCI_USERNAME','username'),env('PTAH_OCI_PASSWORD','password'),env('PTAH_OCI_REGISTRY','registry')],'securityContext':{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}},'volumeMounts':[{'name':'schema','mountPath':'/schema','readOnly':True},{'name':'work','mountPath':'/work'}]}
print(json.dumps({'apiVersion':'batch/v1','kind':'Job','metadata':{'namespace':namespace,'name':'result-schema-publish'},'spec':{'backoffLimit':0,'activeDeadlineSeconds':300,'template':{'spec':{'restartPolicy':'Never','automountServiceAccountToken':False,'securityContext':{'runAsNonRoot':True,'runAsUser':65532,'runAsGroup':65532,'fsGroup':65532,'seccompProfile':{'type':'RuntimeDefault'}},'containers':[container],'volumes':[{'name':'schema','configMap':{'name':'result-schema'}},{'name':'work','emptyDir':{'sizeLimit':'64Mi'}}]}}}}))
PY
k -n "$E2E_TEST_NAMESPACE" wait --for=condition=complete job/result-schema-publish --timeout=300s
k -n "$E2E_TEST_NAMESPACE" logs job/result-schema-publish > /private/tmp/ptah-result-136-publish.log
artifact_digest=$(sed -n 's/^Digest: //p' /private/tmp/ptah-result-136-publish.log)
printf '%s\n' "$artifact_digest" | grep -Eq '^sha256:[a-f0-9]{64}$'
APPLY=Always INTERVAL=2h demo/bin/lab manifest storefront "$artifact_digest" > /private/tmp/ptah-result-136-schema.yaml
k apply -f /private/tmp/ptah-result-136-schema.yaml
k -n "$E2E_TEST_NAMESPACE" wait --for=condition=InSync ptahschema/storefront --timeout=480s
k -n "$E2E_TEST_NAMESPACE" get ptahschema storefront -o json > /private/tmp/ptah-result-136-schema.json
k -n "$E2E_TEST_NAMESPACE" get jobs -o json > /private/tmp/ptah-result-136-jobs.json
k -n "$E2E_TEST_NAMESPACE" get ptahresultrecords -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,type:.spec.type}]' > /private/tmp/ptah-result-136-record-inventory.json
k -n "$E2E_TEST_NAMESPACE" exec deploy/demo-psql -- psql -At -c "SELECT table_name FROM information_schema.tables WHERE table_schema='public' ORDER BY table_name" > /private/tmp/ptah-result-136-database.txt
printf 'Durable workflow reached InSync; database tables:\n'
cat /private/tmp/ptah-result-136-database.txt
