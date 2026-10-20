#!/usr/bin/env bash
set -Eeuo pipefail

: "${GITHUB_WORKSPACE:?}"
: "${GITHUB_ENV:?}"
: "${FUNNEL_CI_IMAGE_REF:?}"
: "${FUNNEL_CI_IMAGE_ID:?}"
: "${FUNNEL_CI_IMAGE_DIGEST:?}"
: "${FUNNEL_CI_IMAGE_CONFIG_ID:?}"
: "${FUNNEL_CI_PLUGIN_IMAGE_REF:?}"
: "${FUNNEL_CI_PLUGIN_IMAGE_ID:?}"
: "${FUNNEL_CI_PLUGIN_IMAGE_DIGEST:?}"
: "${FUNNEL_CI_PLUGIN_IMAGE_CONFIG_ID:?}"
: "${FUNNEL_CI_PLUGIN_SOURCE_SHA:?}"
: "${NAMESPACE:?}"

source_sha=$(git -C "$GITHUB_WORKSPACE" rev-parse HEAD)
if [[ "$source_sha" != "$FUNNEL_CI_SOURCE_SHA" ]]; then
  printf 'checked-out source changed from %s to %s during Gen3 setup\n' "$FUNNEL_CI_SOURCE_SHA" "$source_sha" >&2
  exit 1
fi

for node in kind-cluster-control-plane kind-cluster-worker; do
  if ! docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$FUNNEL_CI_IMAGE_REF" '$1 == image { found = 1 } END { exit !found }'; then
    printf 'candidate image is missing from Kind node %s\n' "$node" >&2
    exit 1
  fi
  digest=$(docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$FUNNEL_CI_IMAGE_REF" '$1 == image { digest = $3 } END { print digest }')
  printf 'loaded_node=%s image=%s digest=%s\n' "$node" "$FUNNEL_CI_IMAGE_REF" "$digest"
  if ! docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$FUNNEL_CI_PLUGIN_IMAGE_REF" '$1 == image { found = 1 } END { exit !found }'; then
    printf 'Gen3 plugin image is missing from Kind node %s\n' "$node" >&2
    exit 1
  fi
  plugin_digest=$(docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$FUNNEL_CI_PLUGIN_IMAGE_REF" '$1 == image { digest = $3 } END { print digest }')
  plugin_config_id=$(docker exec "$node" crictl inspecti "$FUNNEL_CI_PLUGIN_IMAGE_REF" | jq -er '.status.id')
  if [[ "$plugin_config_id" != "$FUNNEL_CI_PLUGIN_IMAGE_ID" || "$plugin_digest" != "$FUNNEL_CI_PLUGIN_IMAGE_DIGEST" ]]; then
    printf 'Gen3 plugin image identity differs on Kind node %s\n' "$node" >&2
    exit 1
  fi
  printf 'loaded_node=%s image=%s digest=%s config_id=%s\n' "$node" "$FUNNEL_CI_PLUGIN_IMAGE_REF" "$plugin_digest" "$plugin_config_id"
done

deployment=$(kubectl -n "$NAMESPACE" get deployment funnel-server -o json)
if ! jq -e --arg image "$FUNNEL_CI_IMAGE_REF" '
  any(.spec.template.spec.containers[]; .name == "funnel" and .image == $image and .imagePullPolicy == "Never")
' <<<"$deployment" >/dev/null; then
  printf 'Funnel server deployment does not select the loaded candidate image with pull disabled\n' >&2
  exit 1
fi
if ! jq -e --arg image "$FUNNEL_CI_PLUGIN_IMAGE_REF" '
  any(.spec.template.spec.initContainers[]; .name == "plugin" and .image == $image and .imagePullPolicy == "Never")
' <<<"$deployment" >/dev/null; then
  printf 'Funnel deployment does not select the loaded Gen3 plugin with pulls disabled\n' >&2
  exit 1
fi

pods=$(kubectl -n "$NAMESPACE" get pods -l app=funnel -o json)
if ! jq -e --arg image "$FUNNEL_CI_IMAGE_REF" --arg config_id "$FUNNEL_CI_IMAGE_CONFIG_ID" --arg digest "$FUNNEL_CI_IMAGE_DIGEST" '
  [.items[] | select(any(.spec.containers[]; .name == "funnel"))] as $pods |
  ($pods | length) > 0 and
  all($pods[];
    any(.spec.containers[]; .name == "funnel" and .image == $image and .imagePullPolicy == "Never") and
    any((.status.containerStatuses // [])[];
      .name == "funnel" and
      ((.imageID | endswith($config_id)) or (.imageID | endswith($digest)))
    )
  )
' <<<"$pods" >/dev/null; then
  printf 'Funnel server pod image IDs do not match the exact candidate image loaded into Kind\n' >&2
  exit 1
fi
if ! jq -e --arg image "$FUNNEL_CI_PLUGIN_IMAGE_REF" --arg config_id "$FUNNEL_CI_PLUGIN_IMAGE_CONFIG_ID" --arg digest "$FUNNEL_CI_PLUGIN_IMAGE_DIGEST" '
  [.items[] | select(any(.spec.initContainers[]?; .name == "plugin"))] as $pods |
  ($pods | length) > 0 and
  all($pods[];
    any(.spec.initContainers[]; .name == "plugin" and .image == $image and .imagePullPolicy == "Never") and
    any((.status.initContainerStatuses // [])[];
      .name == "plugin" and
      ((.imageID | endswith($config_id)) or (.imageID | endswith($digest)))
    )
  )
' <<<"$pods" >/dev/null; then
  printf 'Funnel plugin init-container image IDs do not match the exact pinned plugin image loaded into Kind\n' >&2
  exit 1
fi

worker_template=$(kubectl -n "$NAMESPACE" get configmap funnel-server-config -o json | \
  jq -r '.data["funnel-server.yaml"]' | \
  yq eval -r '.Kubernetes.WorkerTemplate' -)
if [[ "$worker_template" != *'image: {{.Image}}'* || "$worker_template" != *'imagePullPolicy: Never'* || "$worker_template" == *'imagePullPolicy: Always'* ]]; then
  printf 'Funnel worker template does not inherit the candidate server image without pulling\n' >&2
  exit 1
fi

mkdir -p "$GITHUB_WORKSPACE/logs"
jq -n \
  --arg source_sha "$source_sha" \
  --arg image_ref "$FUNNEL_CI_IMAGE_REF" \
  --arg docker_image_id "$FUNNEL_CI_IMAGE_ID" \
  --arg image_digest "$FUNNEL_CI_IMAGE_DIGEST" \
  --arg image_config_id "$FUNNEL_CI_IMAGE_CONFIG_ID" \
  --arg plugin_source_sha "$FUNNEL_CI_PLUGIN_SOURCE_SHA" \
  --arg plugin_image_ref "$FUNNEL_CI_PLUGIN_IMAGE_REF" \
  --arg plugin_image_id "$FUNNEL_CI_PLUGIN_IMAGE_ID" \
  --arg plugin_image_digest "$FUNNEL_CI_PLUGIN_IMAGE_DIGEST" \
  --arg plugin_image_config_id "$FUNNEL_CI_PLUGIN_IMAGE_CONFIG_ID" \
  --argjson deployment "$deployment" \
  --argjson pods "$pods" \
  '{source_sha: $source_sha, image_ref: $image_ref, docker_image_id: $docker_image_id, image_digest: $image_digest, image_config_id: $image_config_id, plugin: {source_sha: $plugin_source_sha, image_ref: $plugin_image_ref, docker_image_id: $plugin_image_id, image_digest: $plugin_image_digest, image_config_id: $plugin_image_config_id}, server: {deployment_image: [$deployment.spec.template.spec.containers[] | select(.name == "funnel") | .image], plugin_image: [$deployment.spec.template.spec.initContainers[] | select(.name == "plugin") | .image], pods: [$pods.items[] | select(any(.spec.containers[]; .name == "funnel")) | {name: .metadata.name, node: .spec.nodeName, containers: [.status.containerStatuses[]? | select(.name == "funnel") | {image, imageID}], initContainers: [.status.initContainerStatuses[]? | select(.name == "plugin") | {image, imageID}]}]}}' \
  >"$GITHUB_WORKSPACE/logs/gen3-image-identity.json"
cat "$GITHUB_WORKSPACE/logs/gen3-image-identity.json"

setup_dir=$(mktemp -d)
trap 'rm -rf "$setup_dir"' EXIT
curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/uc-cdis/gen3-workflow/500b11d7b132518522480fda9abaa1c5cd9a73e2/.github/workflows/integration_tests_on_kind/ci_expose_kind_cluster.sh \
  --output "$setup_dir/ci_expose_kind_cluster.sh"
bash "$setup_dir/ci_expose_kind_cluster.sh"

# Pytest worker crashes discard captured task logs. Keep a bounded independent
# sample in the log that the reusable workflow prints even when a worker dies.
nohup bash "$GITHUB_WORKSPACE/tests/gen3/collect-task-diagnostics.sh" \
  >>"$GITHUB_WORKSPACE/logs/gh_action_logs.txt" 2>&1 </dev/null &
