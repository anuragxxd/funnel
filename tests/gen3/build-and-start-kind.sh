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
if [[ ! "$build_platform" =~ ^linux/(amd64|arm64)$ ]]; then
  printf 'unsupported Gen3 build platform: %s\n' "$build_platform" >&2
  exit 1
fi
target_arch="${build_platform#linux/}"
kind_cluster=kind-cluster
kind_node_image='kindest/node:v1.35.0@sha256:4613778f3cfcd10e615029370f5786704559103cf27bef934597ba562b269661'
task_id="${GITHUB_RUN_ID:-local-${source_sha:0:12}}-${GITHUB_RUN_ATTEMPT:-1}"
state_dir="${FUNNEL_CI_STATE_DIR:-${RUNNER_TEMP:-/tmp}/funnel-gen3-${task_id}}"
kubeconfig="$state_dir/kubeconfig"
go_cache_dir="${FUNNEL_CI_GO_CACHE_DIR:-${RUNNER_TEMP:-/tmp}/funnel-gen3-go-cache}"
go_build_cache="$go_cache_dir/build"
go_module_cache="$go_cache_dir/module"
mkdir -p "$state_dir" "$go_build_cache" "$go_module_cache"
setup_dir=$(mktemp -d)
kind_config="$setup_dir/kind.yaml"
kind_log="$setup_dir/kind-create.log"
builder_log="$setup_dir/go-builder-build.log"
image_archive="$setup_dir/candidate-image.tar"
plugin_archive="$setup_dir/gen3-plugin.tar.gz"
plugin_context="$setup_dir/gen3-plugin"
plugin_image_archive="$setup_dir/gen3-plugin-image.tar"
plugin_output="$setup_dir/gen3-plugin-output"
build_context="$setup_dir/build-context"
builder_dockerfile="$workspace/tests/gen3/image-build/Dockerfile"
runtime_dockerfile="$setup_dir/Dockerfile.funnel-runtime"
builder_image="funnel-gen3-go-builder:ci-${source_sha:0:12}-${target_arch}"
build_pid=""
kind_pid=""

cleanup() {
  status=$?
  trap - EXIT
  for pid in "$build_pid" "$kind_pid"; do
    if [[ -n "$pid" ]]; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if (( status != 0 )); then
    if [[ -f "$builder_log" ]]; then
      tail -n 30 "$builder_log"
    fi
    if [[ -f "$kind_log" ]]; then
      tail -n 30 "$kind_log"
    fi
  fi
  rm -rf "$setup_dir"
  exit "$status"
}
trap cleanup EXIT

printf 'source_commit=%s\ncandidate_image=%s\nbuild_platform=%s\n' "$source_sha" "$candidate_image" "$build_platform"
printf 'gen3_plugin_commit=%s\ngen3_plugin_image=%s\n' "$plugin_source_sha" "$plugin_image"
printf 'go_cache_dir=%s\nkubeconfig=%s\nkind_node_image=%s\n' "$go_cache_dir" "$kubeconfig" "$kind_node_image"
export KUBECONFIG="$kubeconfig"
printf 'FUNNEL_CI_KUBECONFIG=%s\n' "$kubeconfig" >>"$GITHUB_ENV"

if kind get clusters | awk -v name="$kind_cluster" '$0 == name { found = 1 } END { exit !found }'; then
  printf 'Kind cluster %s already exists; refusing to reuse or change it\n' "$kind_cluster" >&2
  exit 1
fi

cat >"$kind_config" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  image: $kind_node_image
- role: worker
  image: $kind_node_image
EOF

mkdir -p "$build_context"
git -C "$workspace" archive --format=tar HEAD | tar -xf - -C "$build_context"

go_builder_image=$(awk '$1 == "FROM" && $3 == "AS" && $4 == "build-env" { print $2; exit }' "$build_context/Dockerfile")
if [[ -z "$go_builder_image" ]]; then
  printf 'could not find the production build-env Go image in Dockerfile\n' >&2
  exit 1
fi
nerdctl_version=$(awk '$1 == "ARG" && $2 ~ /^NERDCTL_VERSION=/ { sub(/^NERDCTL_VERSION=/, "", $2); print $2; exit }' "$build_context/Dockerfile")
if [[ -z "$nerdctl_version" ]]; then
  printf 'could not find the production nerdctl version in Dockerfile\n' >&2
  exit 1
fi

python3 - "$build_context/Dockerfile" "$runtime_dockerfile" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1]).read_text()
marker = "# final stage\n"
if source.count(marker) != 1:
    raise SystemExit("The production Dockerfile final-stage marker changed")
runtime = source.split(marker, 1)[1]
lines = runtime.splitlines()
binary_copies = [i for i, line in enumerate(lines) if line.startswith("COPY --from=build-env ") and line.endswith("/funnel /app/")]
nerdctl_copies = [i for i, line in enumerate(lines) if line == "COPY --from=build-env /tmp/nerdctl /usr/local/bin/nerdctl"]
if len(binary_copies) != 1 or len(nerdctl_copies) != 1:
    raise SystemExit("The production runtime image COPY instructions changed")
lines[binary_copies[0]] = "COPY funnel /app/"
lines[nerdctl_copies[0]] = "COPY nerdctl /usr/local/bin/nerdctl"
Path(sys.argv[2]).write_text("\n".join(lines) + "\n")
PY

build_images() {
  local builder_started builder_ready candidate_compile_started candidate_compile_finished
  local candidate_package_started candidate_package_finished plugin_build_started plugin_build_finished
  local funnel_module plugin_funnel_module

  builder_started=$(date +%s)
  DOCKER_BUILDKIT=1 docker build --platform "$build_platform" \
    --file "$builder_dockerfile" --target go-builder \
    --build-arg "GO_BUILDER_IMAGE=$go_builder_image" \
    --build-arg "WORKSPACE_DIR=/workspace" \
    --build-arg "NERDCTL_VERSION=$nerdctl_version" \
    --build-arg "TARGETARCH=$target_arch" \
    --tag "$builder_image" "$build_context"
  builder_ready=$(date +%s)

  candidate_compile_started=$(date +%s)
  docker run --rm --platform "$build_platform" \
    --volume "$build_context:/workspace" \
    --volume "$go_build_cache:/root/.cache/go-build" \
    --volume "$go_module_cache:/go/pkg/mod" \
    --env GOCACHE=/root/.cache/go-build \
    --env GOPATH=/go \
    --workdir /workspace \
    "$builder_image" \
    bash -euo pipefail -c 'sh .github/scripts/download-go-modules.sh && make build && cp /tmp/nerdctl nerdctl'
  candidate_compile_finished=$(date +%s)

  candidate_package_started=$(date +%s)
  DOCKER_BUILDKIT=1 docker build --platform "$build_platform" \
    --file "$runtime_dockerfile" \
    --label "org.opencontainers.image.revision=$source_sha" \
    --tag "$candidate_image" "$build_context"
  candidate_package_finished=$(date +%s)

  mkdir -p "$plugin_context" "$plugin_output"
  curl --fail --silent --show-error --location --retry 3 --max-time 60 \
    "https://github.com/calypr/funnel-gen3-plugin/archive/${plugin_source_sha}.tar.gz" \
    --output "$plugin_archive"
  tar -xzf "$plugin_archive" --strip-components=1 -C "$plugin_context"

  funnel_module=$(awk '$1 == "module" { module = $2 } END { print module }' "$build_context/go.mod")
  if [[ -z "$funnel_module" ]]; then
    printf 'could not determine current Funnel module path\n' >&2
    return 1
  fi
  plugin_funnel_module=$(awk '$1 == "github.com/ohsu-comp-bio/funnel" || $1 == "github.com/calypr/funnel" { print $1; exit }' "$plugin_context/go.mod")
  if [[ -z "$plugin_funnel_module" ]]; then
    printf 'Gen3 plugin go.mod does not declare a supported Funnel module dependency\n' >&2
    return 1
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

  plugin_build_started=$(date +%s)
  docker run --rm --platform "$build_platform" \
    --volume "$build_context:/workspace/funnel:ro" \
    --volume "$plugin_context:/workspace/plugin" \
    --volume "$plugin_output:/out" \
    --volume "$go_build_cache:/root/.cache/go-build" \
    --volume "$go_module_cache:/go/pkg/mod" \
    --env GOCACHE=/root/.cache/go-build \
    --env GOPATH=/go \
    --env "FUNNEL_MODULE=$funnel_module" \
    --env FUNNEL_MODULE_DIR=/workspace/funnel \
    --workdir /workspace/plugin \
    "$builder_image" \
    bash -euo pipefail -c '
      go mod edit "-replace=${FUNNEL_MODULE}=${FUNNEL_MODULE_DIR}"
      for attempt in 1 2 3; do
        if go mod tidy; then break; fi
        test "$attempt" -lt 3 || exit 1
        sleep "$attempt"
      done
      CGO_ENABLED=0 go build -trimpath -o /out/authorizer ./plugin-gen3
    '
  cp "$plugin_output/authorizer" "$build_context/authorizer"
  DOCKER_BUILDKIT=1 docker build --platform "$build_platform" \
    --file "$builder_dockerfile" --target gen3-plugin \
    --build-arg "GO_BUILDER_IMAGE=$go_builder_image" \
    --label "org.opencontainers.image.revision=$plugin_source_sha" \
    --label "org.opencontainers.image.source=https://github.com/calypr/funnel-gen3-plugin" \
    --tag "$plugin_image" "$build_context"
  plugin_build_finished=$(date +%s)

  docker save "$candidate_image" >"$image_archive"
  docker save "$plugin_image" >"$plugin_image_archive"
  {
    printf 'go_builder_setup_seconds=%s\n' "$((builder_ready - builder_started))"
    printf 'candidate_compile_seconds=%s\n' "$((candidate_compile_finished - candidate_compile_started))"
    printf 'candidate_package_seconds=%s\n' "$((candidate_package_finished - candidate_package_started))"
    printf 'candidate_build_seconds=%s\n' "$((candidate_package_finished - candidate_compile_started))"
    printf 'gen3_plugin_build_seconds=%s\n' "$((plugin_build_finished - plugin_build_started))"
  } >"$setup_dir/build-timings"
}

build_images >"$builder_log" 2>&1 &
build_pid=$!
kind_started=$(date +%s)
{
  printf 'FUNNEL_CI_CLUSTER_OWNED=true\n'
  printf 'FUNNEL_CI_KIND_CLUSTER_NAME=%s\n' "$kind_cluster"
} >>"$GITHUB_ENV"
kind create cluster --name "$kind_cluster" --config "$kind_config" --kubeconfig "$kubeconfig" --wait 180s >"$kind_log" 2>&1 &
kind_pid=$!

if ! wait "$kind_pid"; then
  kind_pid=""
  exit 1
fi
kind_pid=""
cluster_ready=$(date +%s)

if ! wait "$build_pid"; then
  build_pid=""
  exit 1
fi
build_pid=""

kind get nodes --name "$kind_cluster"
candidate_image_id=$(docker image inspect "$candidate_image" --format '{{.Id}}')
candidate_revision=$(docker image inspect "$candidate_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$candidate_revision" != "$source_sha" ]]; then
  printf 'candidate image revision %s does not match checked-out commit %s\n' "$candidate_revision" "$source_sha" >&2
  exit 1
fi
plugin_image_id=$(docker image inspect "$plugin_image" --format '{{.Id}}')
plugin_revision=$(docker image inspect "$plugin_image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$plugin_revision" != "$plugin_source_sha" ]]; then
  printf 'plugin image revision %s does not match pinned source %s\n' "$plugin_revision" "$plugin_source_sha" >&2
  exit 1
fi
cat "$setup_dir/build-timings"

import_started=$(date +%s)
node_digests=()
node_config_ids=()
plugin_node_digests=()
plugin_node_config_ids=()
for node in "$kind_cluster-control-plane" "$kind_cluster-worker"; do
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
printf 'kind_setup_seconds=%s\n' "$((cluster_ready - kind_started))"
cat "$setup_dir/build-timings"
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
