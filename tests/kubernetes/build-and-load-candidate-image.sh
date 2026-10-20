#!/usr/bin/env bash
set -Eeuo pipefail

: "${GITHUB_WORKSPACE:?}"
: "${GITHUB_ENV:?}"

workspace=$(cd "$GITHUB_WORKSPACE" && pwd)
source_sha=$(git -C "$workspace" rev-parse HEAD)
candidate_image="quay.io/ohsu-comp-bio/funnel:ci-${source_sha:0:12}"

printf 'source_commit=%s\ncandidate_image=%s\n' "$source_sha" "$candidate_image"
DOCKER_BUILDKIT=1 docker build \
  --platform linux/amd64 \
  --label "org.opencontainers.image.revision=$source_sha" \
  --tag "$candidate_image" \
  "$workspace"

candidate_image_id=$(docker image inspect "$candidate_image" --format '{{.Id}}')
candidate_revision=$(docker image inspect "$candidate_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$candidate_revision" != "$source_sha" ]]; then
  printf 'candidate image revision %s does not match checked-out commit %s\n' "$candidate_revision" "$source_sha" >&2
  exit 1
fi

nodes=$(kind get nodes --name funnel-gen3)
if [[ -z "$nodes" ]]; then
  printf 'Kind cluster funnel-gen3 has no nodes\n' >&2
  exit 1
fi

while IFS= read -r node; do
  [[ -n "$node" ]] || continue
  docker save "$candidate_image" | docker exec -i "$node" ctr --namespace k8s.io images import --snapshotter overlayfs -
  node_config_id=$(docker exec "$node" crictl inspecti "$candidate_image" | jq -er '.status.id')
  if [[ "$node_config_id" != "$candidate_image_id" ]]; then
    printf 'node %s loaded image ID %s, expected %s\n' "$node" "$node_config_id" "$candidate_image_id" >&2
    exit 1
  fi
  printf 'node=%s image=%s image_id=%s\n' "$node" "$candidate_image" "$node_config_id"
done <<<"$nodes"

{
  printf 'FUNNEL_CI_IMAGE_REF=%s\n' "$candidate_image"
  printf 'FUNNEL_CI_IMAGE_ID=%s\n' "$candidate_image_id"
  printf 'FUNNEL_CI_SOURCE_SHA=%s\n' "$source_sha"
} >>"$GITHUB_ENV"
