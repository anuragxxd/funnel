#!/usr/bin/env bash
set -Eeuo pipefail

: "${GITHUB_WORKSPACE:?}"
: "${GITHUB_ENV:?}"

workspace=$(cd "$GITHUB_WORKSPACE" && pwd)
source_sha=$(git -C "$workspace" rev-parse HEAD)
candidate_image="quay.io/ohsu-comp-bio/funnel:ci-${source_sha:0:12}"
build_platform="${FUNNEL_CI_BUILD_PLATFORM:-linux/amd64}"
setup_dir=$(mktemp -d)
setup_script="$setup_dir/ci_start_kind_cluster.sh"
build_log="$setup_dir/docker-build.log"
build_finished="$setup_dir/docker-build-finished"
image_archive="$setup_dir/candidate-image.tar"
build_context="$setup_dir/build-context"
build_pid=""

cleanup() {
  status=$?
  trap - EXIT
  if [[ -n "$build_pid" ]]; then
    kill "$build_pid" 2>/dev/null || true
    wait "$build_pid" 2>/dev/null || true
  fi
  if [[ -f "$build_log" ]]; then
    tail -n 30 "$build_log"
  fi
  rm -rf "$setup_dir"
  exit "$status"
}
trap cleanup EXIT

printf 'source_commit=%s\ncandidate_image=%s\nbuild_platform=%s\n' "$source_sha" "$candidate_image" "$build_platform"
curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/uc-cdis/gen3-workflow/500b11d7b132518522480fda9abaa1c5cd9a73e2/.github/workflows/integration_tests_on_kind/ci_start_kind_cluster.sh \
  --output "$setup_script"

python3 - "$setup_script" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
source = path.read_text()
prune = "    sudo docker image prune --all --force || true\n"
if source.count(prune) != 1:
    raise SystemExit("The pinned Kind setup script changed its Docker image-prune command")
path.write_text(source.replace(prune, ""))
PY

mkdir -p "$build_context"
git -C "$workspace" archive --format=tar HEAD | tar -xf - -C "$build_context"

build_started=$(date +%s)
(
  status=0
  DOCKER_BUILDKIT=1 docker build --platform "$build_platform" \
    --label "org.opencontainers.image.revision=$source_sha" \
    --tag "$candidate_image" "$build_context" >"$build_log" 2>&1 || status=$?
  printf '%s %s\n' "$(date +%s)" "$status" >"$build_finished"
  exit "$status"
) &
build_pid=$!

cluster_started=$(date +%s)
bash "$setup_script"
cluster_ready=$(date +%s)

if ! wait "$build_pid"; then
  build_pid=""
  exit 1
fi
build_pid=""
read -r build_finished build_status <"$build_finished"
if [[ "$build_status" != "0" ]]; then
  exit "$build_status"
fi

candidate_image_id=$(docker image inspect "$candidate_image" --format '{{.Id}}')
candidate_revision=$(docker image inspect "$candidate_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$candidate_revision" != "$source_sha" ]]; then
  printf 'candidate image revision %s does not match checked-out commit %s\n' "$candidate_revision" "$source_sha" >&2
  exit 1
fi

docker save "$candidate_image" >"$image_archive"
import_started=$(date +%s)
node_digests=()
node_config_ids=()
for node in kind-cluster-control-plane kind-cluster-worker; do
  docker exec -i "$node" ctr --namespace k8s.io images import --snapshotter overlayfs - <"$image_archive"
  if ! docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$candidate_image" '$1 == image { found = 1 } END { exit !found }'; then
    printf 'candidate image was not imported into Kind node %s\n' "$node" >&2
    exit 1
  fi
  digest=$(docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$candidate_image" '$1 == image { digest = $3 } END { print digest }')
  config_id=$(docker exec "$node" crictl inspecti "$candidate_image" | jq -er '.status.id')
  node_digests+=("$digest")
  node_config_ids+=("$config_id")
  printf 'node=%s image=%s digest=%s config_id=%s\n' "$node" "$candidate_image" "$digest" "$config_id"
done
if [[ "${node_digests[0]}" != "${node_digests[1]}" || "${node_config_ids[0]}" != "$candidate_image_id" || "${node_config_ids[1]}" != "$candidate_image_id" ]]; then
  printf 'Kind nodes did not import the exact candidate image identity\n' >&2
  exit 1
fi
import_finished=$(date +%s)

printf 'candidate_image_id=%s\n' "$candidate_image_id"
printf 'kind_setup_seconds=%s\n' "$((cluster_ready - cluster_started))"
printf 'candidate_build_seconds=%s\n' "$((build_finished - build_started))"
printf 'candidate_import_seconds=%s\n' "$((import_finished - import_started))"
{
  printf 'FUNNEL_CI_IMAGE_REF=%s\n' "$candidate_image"
  printf 'FUNNEL_CI_IMAGE_ID=%s\n' "$candidate_image_id"
  printf 'FUNNEL_CI_IMAGE_DIGEST=%s\n' "${node_digests[0]}"
  printf 'FUNNEL_CI_IMAGE_CONFIG_ID=%s\n' "${node_config_ids[0]}"
  printf 'FUNNEL_CI_SOURCE_SHA=%s\n' "$source_sha"
} >>"$GITHUB_ENV"
