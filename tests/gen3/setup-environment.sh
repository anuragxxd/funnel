#!/usr/bin/env bash
set -Eeuo pipefail

setup_dir=$(mktemp -d)
setup_script="$setup_dir/setup.sh"
cleanup() {
  status=$?
  trap - EXIT
  rm -rf "$setup_dir"
  if (( status != 0 )); then
    kubectl get nodes -o wide || true
    kubectl get pods -A -o wide || true
    kubectl get events -A --sort-by='.lastTimestamp' || true
    while read -r pod; do
      kubectl -n kube-system describe "$pod" || true
      kubectl -n kube-system logs "$pod" --all-containers --prefix --tail=100 || true
      kubectl -n kube-system logs "$pod" --all-containers --prefix --previous --tail=100 || true
    done < <(kubectl -n kube-system get pods -o name | awk '/\/s3-csi-/')
  fi
  exit "$status"
}
trap cleanup EXIT

: "${GITHUB_WORKSPACE:?GitHub workspace path is required}"
test_repo="${TEST_REPO:?pinned Gen3 test repository checkout is required}"
fixture_values="$GITHUB_WORKSPACE/tests/gen3/values-ci.yaml"
setup_ci_env="$test_repo/gen3-integration-tests/gen3_ci/scripts/setup_ci_env.sh"
renderer="$GITHUB_WORKSPACE/tests/gen3/gen3-post-renderer.sh"
csi_images="$GITHUB_WORKSPACE/tests/gen3/csi-images.yaml"
if [[ ! -f "$fixture_values" || ! -f "$setup_ci_env" ]]; then
  printf 'Gen3 fixture values or pinned test setup helper is missing\n' >&2
  exit 1
fi
if [[ ! -f "$csi_images" ]]; then
  printf 'owned Mountpoint CSI image manifest is missing\n' >&2
  exit 1
fi

curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/uc-cdis/gen3-workflow/500b11d7b132518522480fda9abaa1c5cd9a73e2/.github/workflows/integration_tests_on_kind/ci_override_config_and_start_minio.sh \
  --output "$setup_script"

cp "$csi_images" "$setup_dir/csi-images.yaml"

: "${FUNNEL_CI_IMAGE_REF:?candidate image must be built and loaded before Gen3 setup}"
: "${FUNNEL_CI_IMAGE_ID:?candidate image identity must be recorded before Gen3 setup}"
: "${FUNNEL_CI_SOURCE_SHA:?candidate image source commit must be recorded before Gen3 setup}"
: "${FUNNEL_CI_PLUGIN_IMAGE_REF:?Gen3 plugin image must be built and loaded before Gen3 setup}"
: "${FUNNEL_CI_PLUGIN_IMAGE_ID:?Gen3 plugin image identity must be recorded before Gen3 setup}"
: "${FUNNEL_CI_PLUGIN_SOURCE_SHA:?Gen3 plugin source commit must be recorded before setup}"

candidate_image_id=$(docker image inspect "$FUNNEL_CI_IMAGE_REF" --format '{{.Id}}')
candidate_revision=$(docker image inspect "$FUNNEL_CI_IMAGE_REF" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$candidate_image_id" != "$FUNNEL_CI_IMAGE_ID" || "$candidate_revision" != "$FUNNEL_CI_SOURCE_SHA" ]]; then
  printf 'candidate image identity does not match the checked-out Funnel source\n' >&2
  exit 1
fi
plugin_image_id=$(docker image inspect "$FUNNEL_CI_PLUGIN_IMAGE_REF" --format '{{.Id}}')
plugin_revision=$(docker image inspect "$FUNNEL_CI_PLUGIN_IMAGE_REF" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$plugin_image_id" != "$FUNNEL_CI_PLUGIN_IMAGE_ID" || "$plugin_revision" != "$FUNNEL_CI_PLUGIN_SOURCE_SHA" ]]; then
  printf 'Gen3 plugin image identity does not match the pinned plugin source\n' >&2
  exit 1
fi

python3 - "$setup_script" "$setup_dir/csi-images.yaml" <<'PY'
from pathlib import Path
import shlex
import sys

path = Path(sys.argv[1])
source = path.read_text()
install = "helm upgrade --install aws-mountpoint-s3-csi-driver --namespace kube-system aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver -f values-kind.yaml"
if source.count(install) != 1:
    raise SystemExit("The pinned upstream CSI install command changed")
path.write_text(source.replace(install, install + " --version 2.8.0 -f " + shlex.quote(sys.argv[2])))
PY

bash "$setup_script"

gen3_chart_version=0.3.110
gen3_ready_timeout_seconds=360
gen3_ready_poll_interval_seconds=5

if [[ "$(helm version --template '{{.Version}}')" == v4.* ]]; then
  helm plugin install "$GITHUB_WORKSPACE/tests/gen3"
  renderer=funnel-ci-image
fi
python3 - "$setup_ci_env" "$renderer" "$fixture_values" "$gen3_chart_version" \
  "$gen3_ready_timeout_seconds" "$gen3_ready_poll_interval_seconds" <<'PY'
from pathlib import Path
import shlex
import sys

path = Path(sys.argv[1])
source = path.read_text()
renderer = " --post-renderer " + shlex.quote(sys.argv[2])
values = " --values " + shlex.quote(sys.argv[3])
chart_version = " --version " + shlex.quote(sys.argv[4])
lines = source.splitlines(keepends=True)
targets = [
    index
    for index, line in enumerate(lines)
    if "helm upgrade --install ${namespace} gen3/gen3 " in line
]
if len(targets) != 1:
    raise SystemExit("The pinned Gen3 setup helper changed its Helm install command")
line = lines[targets[0]]
if renderer.strip() in line:
    raise SystemExit("The Gen3 post-renderer is already present in the Helm command")
if values.strip() in line or chart_version.strip() in line:
    raise SystemExit("The Gen3 fixture values or chart version are already present in the Helm command")
line = line.replace(" gen3/gen3 ", " gen3/gen3" + chart_version + " ", 1)
marker = " --debug; then"
if line.count(marker) != 1:
    raise SystemExit("The pinned Gen3 Helm command changed its debug/install terminator")
line = line.replace(marker, values + renderer + "; then")
lines[targets[0]] = line

source = "".join(lines)
timeout = "export timeout=1800  # 30 min"
interval = "export interval=20"
if source.count(timeout) != 1 or source.count(interval) != 1:
    raise SystemExit("The pinned Gen3 readiness loop changed its timeout or polling interval")
source = source.replace(timeout, f"export timeout={sys.argv[5]}", 1)
source = source.replace(interval, f"export interval={sys.argv[6]}", 1)
path.write_text(source)
PY

if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'SKIP_QUAY_BUILD=true\n' >> "$GITHUB_ENV"
  printf 'PYTEST_ADDOPTS=%s\n' "${PYTEST_ADDOPTS:+$PYTEST_ADDOPTS }--max-worker-restart=0" >> "$GITHUB_ENV"
fi
