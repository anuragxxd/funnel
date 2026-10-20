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

curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/uc-cdis/gen3-workflow/500b11d7b132518522480fda9abaa1c5cd9a73e2/.github/workflows/integration_tests_on_kind/ci_override_config_and_start_minio.sh \
  --output "$setup_script"

curl --fail --silent --show-error --location \
  https://raw.githubusercontent.com/calypr/funnel/fb82e812aa21ce680f553b9170092ed5fb2045f4/tests/gen3/csi-images.yaml \
  --output "$setup_dir/csi-images.yaml"

: "${FUNNEL_CI_IMAGE_REF:?candidate image must be built and loaded before Gen3 setup}"
: "${FUNNEL_CI_IMAGE_ID:?candidate image identity must be recorded before Gen3 setup}"

candidate_image_id=$(docker image inspect "$FUNNEL_CI_IMAGE_REF" --format '{{.Id}}')
candidate_revision=$(docker image inspect "$FUNNEL_CI_IMAGE_REF" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')
if [[ "$candidate_image_id" != "$FUNNEL_CI_IMAGE_ID" || "$candidate_revision" != "$FUNNEL_CI_SOURCE_SHA" ]]; then
  printf 'candidate image identity does not match the checked-out Funnel source\n' >&2
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

setup_ci_env="$GITHUB_WORKSPACE/gen3-code-vigil/gen3-integration-tests/gen3_ci/scripts/setup_ci_env.sh"
renderer="$GITHUB_WORKSPACE/tests/gen3/gen3-post-renderer.sh"
if [[ "$(helm version --template '{{.Version}}')" == v4.* ]]; then
  helm plugin install "$GITHUB_WORKSPACE/tests/gen3"
  renderer=funnel-ci-image
fi
python3 - "$setup_ci_env" "$renderer" <<'PY'
from pathlib import Path
import shlex
import sys

path = Path(sys.argv[1])
source = path.read_text()
renderer = " --post-renderer " + shlex.quote(sys.argv[2])
lines = source.splitlines(keepends=True)
targets = [
    index
    for index, line in enumerate(lines)
    if "helm upgrade --install ${namespace} gen3/gen3 " in line
]
if len(targets) != 1:
    raise SystemExit("The Gen3 setup helper changed its default Helm install command")
line = lines[targets[0]]
if renderer.strip() in line:
    raise SystemExit("The Gen3 post-renderer is already present in the Helm command")
marker = " --debug; then"
if line.count(marker) != 1:
    raise SystemExit("The Gen3 Helm command changed its debug/install terminator")
lines[targets[0]] = line.replace(marker, renderer + marker)
path.write_text("".join(lines))
PY

if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'SKIP_QUAY_BUILD=true\n' >> "$GITHUB_ENV"
  printf 'PYTEST_ADDOPTS=%s\n' "${PYTEST_ADDOPTS:+$PYTEST_ADDOPTS }--max-worker-restart=0" >> "$GITHUB_ENV"
fi
