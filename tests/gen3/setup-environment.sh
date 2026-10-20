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

# The chart and preloaded images must move together.
python3 - "$setup_script" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
source = path.read_text()
install = "helm upgrade --install aws-mountpoint-s3-csi-driver --namespace kube-system aws-mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver -f values-kind.yaml"
if source.count(install) != 1:
    raise SystemExit("The pinned upstream CSI install command changed")
path.write_text(source.replace(install, install + " --version 2.8.0"))
PY

# Pull through host HTTP before the nodes request these images from ECR.
curl --fail --silent --show-error --location --retry 3 \
  https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_x86_64.tar.gz \
  --output "$setup_dir/crane.tar.gz"
printf '%s  %s\n' \
  0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0 \
  "$setup_dir/crane.tar.gz" | sha256sum --check
tar -xzf "$setup_dir/crane.tar.gz" -C "$setup_dir" crane
mkdir "$setup_dir/docker"

nodes=$(kind get nodes --name kind-cluster)
[[ -n "$nodes" ]] || { echo "Kind cluster has no nodes" >&2; exit 1; }
while read -r image digest; do
  repository=${image%:*}
  archive="$setup_dir/image.tar"
  for attempt in 1 2 3; do
    if DOCKER_CONFIG="$setup_dir/docker" "$setup_dir/crane" pull \
      --platform linux/amd64 "$repository@$digest" "$archive"; then
      break
    fi
    if (( attempt == 3 )); then
      echo "Failed to preload $image after $attempt attempts" >&2
      exit 1
    fi
    sleep "$((attempt * 2))"
  done

  while read -r node; do
    # Kind 0.23's loader cannot discover containerd 2's snapshotter.
    docker exec -i "$node" ctr --namespace k8s.io images import \
      --snapshotter overlayfs - < "$archive"
    # Crane gives digest-only pulls this placeholder tag in Docker archives.
    docker exec "$node" ctr --namespace k8s.io images tag --force \
      "$repository:i-was-a-digest" "$image"
  done <<< "$nodes"
  rm "$archive"
done <<'IMAGES'
public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v2.8.0 sha256:515896d39c19515286bbd45dddd95d36f10e1cb8233350bb7c0e00c14bd9b526
public.ecr.aws/csi-components/csi-node-driver-registrar:v2.17.0-eksbuild.5 sha256:55c3eed041215e13100a6a5792b74d473660b3595047f0582fb1bb9d8c918eda
public.ecr.aws/csi-components/livenessprobe:v2.19.0-eksbuild.5 sha256:8e00443e13311e92bbf0227fb98f1ffff417be82f2eae79a67df16736cf1c1a3
IMAGES

bash "$setup_script"

if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'PYTEST_ADDOPTS=%s\n' "${PYTEST_ADDOPTS:+$PYTEST_ADDOPTS }--max-worker-restart=0" >> "$GITHUB_ENV"
fi
