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

if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'PYTEST_ADDOPTS=%s\n' "${PYTEST_ADDOPTS:+$PYTEST_ADDOPTS }--max-worker-restart=0" >> "$GITHUB_ENV"
fi
