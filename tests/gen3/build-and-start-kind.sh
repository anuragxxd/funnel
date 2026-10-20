#!/usr/bin/env bash
set -Eeuo pipefail

: "${GITHUB_WORKSPACE:?}"
: "${GITHUB_ENV:?}"
plugin_source_sha=eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3
if [[ ! "$plugin_source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'invalid Gen3 authorizer plugin commit: %s\n' "$plugin_source_sha" >&2
  exit 1
fi

workspace=$(cd "$GITHUB_WORKSPACE" && pwd)
source_sha=$(git -C "$workspace" rev-parse HEAD)
candidate_image="quay.io/ohsu-comp-bio/funnel:ci-${source_sha:0:12}"
plugin_image="quay.io/ohsu-comp-bio/funnel-gen3-plugin:ci-${source_sha:0:12}-${plugin_source_sha:0:12}"
build_platform="${FUNNEL_CI_BUILD_PLATFORM:-linux/amd64}"
setup_dir=$(mktemp -d)
setup_script="$setup_dir/ci_start_kind_cluster.sh"
build_log="$setup_dir/docker-build.log"
build_finished="$setup_dir/docker-build-finished"
image_archive="$setup_dir/candidate-image.tar"
plugin_archive="$setup_dir/gen3-plugin.tar.gz"
plugin_context="$setup_dir/gen3-plugin"
plugin_image_archive="$setup_dir/gen3-plugin-image.tar"
plugin_dockerfile="$setup_dir/Dockerfile.gen3-plugin"
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
printf 'gen3_plugin_commit=%s\ngen3_plugin_image=%s\n' "$plugin_source_sha" "$plugin_image"
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

mkdir -p "$plugin_context"
curl --fail --silent --show-error --location --retry 3 --max-time 60 \
  "https://github.com/calypr/funnel-gen3-plugin/archive/${plugin_source_sha}.tar.gz" \
  --output "$plugin_archive"
tar -xzf "$plugin_archive" --strip-components=1 -C "$plugin_context"

# Make the plugin consume the module checked out for this PR. Funnel's module
# path changes across the stack, so rewrite only the plugin's Funnel import path
# in this temporary build context to the current candidate module path.
funnel_module=$(awk '$1 == "module" { module = $2 } END { print module }' "$build_context/go.mod")
if [[ -z "$funnel_module" ]]; then
  printf 'could not determine current Funnel module path\n' >&2
  exit 1
fi
plugin_funnel_module=$(awk '$1 == "github.com/ohsu-comp-bio/funnel" || $1 == "github.com/calypr/funnel" { print $1; exit }' "$plugin_context/go.mod")
if [[ -z "$plugin_funnel_module" ]]; then
  printf 'Gen3 plugin go.mod does not declare a supported Funnel module dependency\n' >&2
  exit 1
fi
python3 - "$plugin_context" "$plugin_funnel_module" "$funnel_module" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
old, new = sys.argv[2:]
for path in [root / "go.mod", *root.rglob("*.go")]:
    source = path.read_text()
    if old in source:
        path.write_text(source.replace(old, new))
PY

cat "$build_context/Dockerfile" >"$plugin_dockerfile"
cat >>"$plugin_dockerfile" <<'EOF'

FROM build-env AS plugin-build
ARG FUNNEL_MODULE
COPY --from=gen3-plugin-source . /tmp/gen3-plugin
RUN --mount=type=cache,target=/root/.cache/go-build \
    funnel_module_dir="$(pwd)" && cd /tmp/gen3-plugin && \
    go mod edit "-replace=${FUNNEL_MODULE}=${funnel_module_dir}" && \
    for attempt in 1 2 3; do \
      if go mod tidy; then break; fi; \
      test "$attempt" -lt 3 || exit 1; \
      sleep "$attempt"; \
    done && \
    mkdir -p /out && CGO_ENABLED=0 go build -trimpath -o /out/authorizer ./plugin-gen3

FROM alpine:3.22 AS gen3-plugin
COPY --from=plugin-build /out/authorizer /app/build/plugins/authorizer
EOF
plugin_build_started=$(date +%s)
DOCKER_BUILDKIT=1 docker build --platform "$build_platform" \
  --file "$plugin_dockerfile" --target gen3-plugin \
  --build-context "gen3-plugin-source=$plugin_context" \
  --build-arg "FUNNEL_MODULE=$funnel_module" \
  --label "org.opencontainers.image.revision=$plugin_source_sha" \
  --label "org.opencontainers.image.source=https://github.com/calypr/funnel-gen3-plugin" \
  --tag "$plugin_image" "$build_context"
plugin_build_finished=$(date +%s)
plugin_image_id=$(docker image inspect "$plugin_image" --format '{{.Id}}')
plugin_revision=$(docker image inspect "$plugin_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$plugin_revision" != "$plugin_source_sha" ]]; then
  printf 'plugin image revision %s does not match pinned source %s\n' "$plugin_revision" "$plugin_source_sha" >&2
  exit 1
fi
docker save "$plugin_image" >"$plugin_image_archive"

import_started=$(date +%s)
node_digests=()
node_config_ids=()
plugin_node_digests=()
plugin_node_config_ids=()
for node in kind-cluster-control-plane kind-cluster-worker; do
  docker exec -i "$node" ctr --namespace k8s.io images import --snapshotter overlayfs - <"$image_archive"
  docker exec -i "$node" ctr --namespace k8s.io images import --snapshotter overlayfs - <"$plugin_image_archive"
  if ! docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$candidate_image" '$1 == image { found = 1 } END { exit !found }'; then
    printf 'candidate image was not imported into Kind node %s\n' "$node" >&2
    exit 1
  fi
  digest=$(docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$candidate_image" '$1 == image { digest = $3 } END { print digest }')
  config_id=$(docker exec "$node" crictl inspecti "$candidate_image" | jq -er '.status.id')
  if ! docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$plugin_image" '$1 == image { found = 1 } END { exit !found }'; then
    printf 'Gen3 plugin image was not imported into Kind node %s\n' "$node" >&2
    exit 1
  fi
  plugin_digest=$(docker exec "$node" ctr --namespace k8s.io images ls | awk -v image="$plugin_image" '$1 == image { digest = $3 } END { print digest }')
  plugin_config_id=$(docker exec "$node" crictl inspecti "$plugin_image" | jq -er '.status.id')
  node_digests+=("$digest")
  node_config_ids+=("$config_id")
  plugin_node_digests+=("$plugin_digest")
  plugin_node_config_ids+=("$plugin_config_id")
  printf 'node=%s image=%s digest=%s config_id=%s\n' "$node" "$candidate_image" "$digest" "$config_id"
  printf 'node=%s image=%s digest=%s config_id=%s\n' "$node" "$plugin_image" "$plugin_digest" "$plugin_config_id"
done
if [[ "${node_digests[0]}" != "${node_digests[1]}" || "${node_config_ids[0]}" != "$candidate_image_id" || "${node_config_ids[1]}" != "$candidate_image_id" ]]; then
  printf 'Kind nodes did not import the exact candidate image identity\n' >&2
  exit 1
fi
if [[ "${plugin_node_digests[0]}" != "${plugin_node_digests[1]}" || "${plugin_node_config_ids[0]}" != "$plugin_image_id" || "${plugin_node_config_ids[1]}" != "$plugin_image_id" ]]; then
  printf 'Kind nodes did not import the exact Gen3 plugin image identity\n' >&2
  exit 1
fi
import_finished=$(date +%s)

printf 'candidate_image_id=%s\n' "$candidate_image_id"
printf 'gen3_plugin_image_id=%s\n' "$plugin_image_id"
printf 'kind_setup_seconds=%s\n' "$((cluster_ready - cluster_started))"
printf 'candidate_build_seconds=%s\n' "$((build_finished - build_started))"
printf 'gen3_plugin_build_seconds=%s\n' "$((plugin_build_finished - plugin_build_started))"
printf 'candidate_import_seconds=%s\n' "$((import_finished - import_started))"
{
  printf 'FUNNEL_CI_IMAGE_REF=%s\n' "$candidate_image"
  printf 'FUNNEL_CI_IMAGE_ID=%s\n' "$candidate_image_id"
  printf 'FUNNEL_CI_IMAGE_DIGEST=%s\n' "${node_digests[0]}"
  printf 'FUNNEL_CI_IMAGE_CONFIG_ID=%s\n' "${node_config_ids[0]}"
  printf 'FUNNEL_CI_SOURCE_SHA=%s\n' "$source_sha"
  printf 'FUNNEL_CI_PLUGIN_IMAGE_REF=%s\n' "$plugin_image"
  printf 'FUNNEL_CI_PLUGIN_IMAGE_ID=%s\n' "$plugin_image_id"
  printf 'FUNNEL_CI_PLUGIN_IMAGE_DIGEST=%s\n' "${plugin_node_digests[0]}"
  printf 'FUNNEL_CI_PLUGIN_IMAGE_CONFIG_ID=%s\n' "${plugin_node_config_ids[0]}"
  printf 'FUNNEL_CI_PLUGIN_SOURCE_SHA=%s\n' "$plugin_source_sha"
} >>"$GITHUB_ENV"
