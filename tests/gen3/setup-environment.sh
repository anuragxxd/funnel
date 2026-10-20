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

# The mirror preserves the upstream Linux/amd64 manifest digests.
cat > "$setup_dir/csi-images.yaml" <<'YAML'
image:
  repository: quay.io/ohsu-comp-bio/funnel
  tag: "ci-csi-driver-v2.8.0-linux-amd64-484def4a3b2e@sha256:484def4a3b2eec778ebae1c1574e2b8685df68d316d489fb7dc8d8bef814cb84"
sidecars:
  nodeDriverRegistrar:
    image:
      repository: quay.io/ohsu-comp-bio/funnel
      tag: "ci-csi-registrar-v2.17.0-eksbuild.5-linux-amd64-aa84e8d445a2@sha256:aa84e8d445a27744f21d4eb90dc2810e681e1fe4681f0ec60dd0db9454d4aef2"
  livenessProbe:
    image:
      repository: quay.io/ohsu-comp-bio/funnel
      tag: "ci-csi-liveness-v2.19.0-eksbuild.5-linux-amd64-6bc3e10863a8@sha256:6bc3e10863a825c6d5c48492bce8c4be4a6582aa77289975bccd17ab164590ed"
YAML

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
