# Qualification kindnet correction

The frozen capacity workload retains 1,000 completed Jobs and their Pods.
Their Pod IPs can be reused by operation Jobs. The kindnet image shipped with
kind v0.33.0 prefers a `Running` Pod when resolving an IP, then chooses an
arbitrary match. A Pod executing an init container is still `Pending`, so it
can inherit a completed background Pod's deny policy. The recorded full-profile
cold start failed after 300 seconds because schema and migration fetch init
containers could not reach DNS. See `capacityPendingPodIPConflict` in
[`../evidence-index.json`](../evidence-index.json).

This qualification fixture prefers the live `Pending` Pod before the terminal
fallback. It preserves the existing `Running` preference, policy evaluation,
and terminal-only lookup behavior. It changes no operator image or chart and
removes no background object, network restriction, or acceptance target.

The recipe pins kind source `69b56db75a6e82dece4cdc980655d79b2b73eb77`, its
existing network-policy dependency `f67f0fb35e2b8288782bc1f22988648580dda7f8`,
both source archive checksums, the Go builder, and the original multi-platform
kindnet runtime. The original runtime's licenses remain in the resulting image.
The build runs the upstream Pod resolver tests and the retained-IP regression.
Building an arm64 image on an amd64 machine does not establish native arm64
runtime qualification.

Build and install before starting a measured workload. Use the Docker context
that owns the disposable qualification cluster; keep any existing workload
suspended while replacing its network agent.

```sh
qualification_docker_context=diabolocom
qualification_kindnet_image=ptah-kindnet-qualification:$(git rev-parse HEAD)
docker --context "$qualification_docker_context" build --platform linux/amd64 \
  --tag "$qualification_kindnet_image" \
  --file support/qualification/kindnet/Dockerfile support/qualification/kindnet
DOCKER_CONTEXT="$qualification_docker_context" kind load docker-image \
  --name "$QUALIFICATION_CLUSTER" "$qualification_kindnet_image"
kubectl --kubeconfig "$KUBECONFIG" -n kube-system set image daemonset/kindnet \
  kindnet-cni="$qualification_kindnet_image"
kubectl --kubeconfig "$KUBECONFIG" -n kube-system rollout status daemonset/kindnet \
  --timeout=180s
```

For Linux arm64, build with `--platform linux/arm64` and install on the native
arm64 qualification host. Record the source commit, patch digest, built image
identity, original and resulting DaemonSets, and every active agent's Pod UID
and image identity. Confirm all expected nodes have a Ready replacement agent;
a terminating old Pod is not a replacement. Keep the original DaemonSet for
rollback and remove only task-owned images during final lab cleanup.

Repeat allowed and denied traffic controls after installation, including a
probe in an init container whose IP remains recorded by a completed background
Pod. Both families must then complete the measured workload with the unchanged
object population and limits. Earlier CNI diagnostics establish the cause and
correction only; they do not turn the failed capacity run into a pass.
