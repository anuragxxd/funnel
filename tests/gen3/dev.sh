#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
workspace=$(cd "$script_dir/../.." && pwd)
export FUNNEL_CI_STATE_DIR=${FUNNEL_CI_STATE_DIR:-$workspace/.gen3-local}
mkdir -p "$FUNNEL_CI_STATE_DIR"
export PATH="$FUNNEL_CI_STATE_DIR/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
export GITHUB_WORKSPACE="$workspace"
export GITHUB_ENV="$FUNNEL_CI_STATE_DIR/environment"
export FUNNEL_CI_KUBECONFIG="$FUNNEL_CI_STATE_DIR/kubeconfig"
export KUBECONFIG="$FUNNEL_CI_KUBECONFIG"
export FUNNEL_CI_KIND_CLUSTER_NAME=${FUNNEL_CI_KIND_CLUSTER_NAME:-$(cat "$FUNNEL_CI_STATE_DIR/cluster-name" 2>/dev/null || printf funnel-gen3-local)}
export FUNNEL_CI_REUSE_CLUSTER=true
export FUNNEL_CI_GO_CACHE_DIR="$FUNNEL_CI_STATE_DIR/go-cache"
export TEST_REPO="$FUNNEL_CI_STATE_DIR/checkouts/gen3-code-vigil"
export GEN3_GITOPS_REPO="$FUNNEL_CI_STATE_DIR/checkouts/gen3-gitops-ci"
export GITOPS_REPO="$GEN3_GITOPS_REPO"
export WORKING_DIR="${TEST_REPO}/gen3-integration-tests"
export NAMESPACE=gen3-local
export HOSTNAME=localhost:8000 HOSTNAME_PROTOCOL=http
export POETRY_VIRTUALENVS_PATH="$FUNNEL_CI_STATE_DIR/python/venvs"
export POETRY_CACHE_DIR="$FUNNEL_CI_STATE_DIR/python/cache"
export UV_CACHE_DIR="$FUNNEL_CI_STATE_DIR/python/uv"
export AWS_DEFAULT_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true
export AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null
unset AWS_PROFILE AWS_DEFAULT_PROFILE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
export SERVICE_TO_TEST=gen3_workflow
export GEN3_CHART_VERSION=0.3.110
export GEN3_VALUES_FILE="$script_dir/values-ci.yaml"
export FUNNEL_CI_BUILD_PLATFORM=${FUNNEL_CI_BUILD_PLATFORM:-linux/$(uname -m | sed 's/aarch64/arm64/;s/x86_64/amd64/')}
touch "$GITHUB_ENV"
while IFS='=' read -r key value; do
  if [[ "$key" =~ ^FUNNEL_CI_(IMAGE_[A-Z_]+|PLUGIN_[A-Z_]+|SOURCE_[A-Z_]+)$ ]]; then
    printf -v "$key" '%s' "$value"
    export "$key"
  fi
done <"$GITHUB_ENV"

usage() {
  cat <<'EOF'
Usage: bash tests/gen3/dev.sh <command> [args]

  deps                 Prepare pinned checkouts and Python dependencies once
  build                Build current working files; create or reuse owned Kind
  step <phase>         Run only one bootstrap phase
                       values, render, storage, install, database, auth, users, client, prepare, deploy, dpop
  test <pytest args>   Run a selected test against the existing environment
  forward              Keep the local HTTP port-forward in the foreground
  doctor               Check local cluster and Docker VM disk space
  status               Show existing pods without starting or rebuilding anything
  logs <resource>      Read logs, for example deployment/fence-deployment
  down                 Explicitly delete this checkout's owned cluster

State and logs persist in .gen3-local. No command automatically tears down a
failed environment. Run build only for image changes; step/test never build.
EOF
}

require_owned_cluster() {
  local expected context cluster
  expected=$(printf 'workspace=%s\ncluster=%s\nkubeconfig=%s\n' "$workspace" "$FUNNEL_CI_KIND_CLUSTER_NAME" "$KUBECONFIG")
  [[ -f "$FUNNEL_CI_STATE_DIR/kind-cluster-ownership" &&
    "$(cat "$FUNNEL_CI_STATE_DIR/kind-cluster-ownership")" == "$expected" ]] || {
    printf 'No matching owned cluster. Run build once, or select its existing state directory.\n' >&2
    return 2
  }
  context=$(kubectl config current-context)
  cluster=$(kubectl config view --minify -o jsonpath='{.contexts[0].context.cluster}')
  [[ "$context" == "kind-$FUNNEL_CI_KIND_CLUSTER_NAME" && "$cluster" == "$context" ]] || {
    printf 'Explicit kubeconfig does not match the owned cluster.\n' >&2
    return 2
  }
  export FUNNEL_CI_CLUSTER_OWNED=true
}

check_capacity() {
  local available
  available=$(timeout 5 docker exec "$FUNNEL_CI_KIND_CLUSTER_NAME-worker" df -Pk /var | awk 'NR == 2 { print $4 }')
  [[ "$available" =~ ^[0-9]+$ ]] || { printf 'Cannot read Docker VM free space.\n' >&2; return 2; }
  if (( available < 2097152 )); then
    printf 'Docker VM has only %s KiB available; at least 2 GiB is required for reliable local tests. Free Docker VM space before retrying.\n' "$available" >&2
    return 2
  fi
  printf 'Docker VM available space: %s KiB\n' "$available"
}

checkout_pin() {
  local repo=$1 pin=$2 destination=$3
  if [[ ! -d "$destination/.git" ]]; then
    mkdir -p "$(dirname "$destination")"
    git clone --filter=blob:none --no-checkout "https://github.com/$repo.git" "$destination"
    git -C "$destination" checkout --detach "$pin"
  fi
  [[ "$(git -C "$destination" rev-parse HEAD)" == "$pin" ]] || {
    printf 'Checkout %s differs from pin %s; refusing to reset local changes.\n' "$destination" "$pin" >&2
    return 2
  }
}

command_name=${1:-help}
[[ $# == 0 ]] || shift
case "$command_name" in
  help|--help|-h) usage; exit 0 ;;
  deps|build|step|test|forward|doctor|status|logs|down) ;;
  *) usage >&2; exit 2 ;;
esac

run_dir=$(mktemp -d "$FUNNEL_CI_STATE_DIR/run-${command_name}-XXXXXX")
export FUNNEL_CI_ARTIFACTS_DIR="$run_dir"
started=$SECONDS
finish() {
  local status=$?
  printf 'command=%s exit=%s elapsed_seconds=%s logs=%s\n' "$command_name" "$status" "$((SECONDS-started))" "$run_dir" | tee "$run_dir/result.txt"
}
trap finish EXIT

case "$command_name" in
  deps)
    checkout_pin uc-cdis/gen3-code-vigil 87da170a9a1cb099cfd1aded9402bc323201d6bc "$TEST_REPO"
    checkout_pin uc-cdis/gen3-gitops 19a687205de78d985416f44ad89c79bd2e66dab8 "$GEN3_GITOPS_REPO"
    python3 "$script_dir/configure-tests.py" "$WORKING_DIR"
    uv tool run --python 3.13 --from poetry==2.2.1 poetry -C "$WORKING_DIR" env use "$(uv python find 3.13)"
    uv tool run --python 3.13 --from poetry==2.2.1 poetry -C "$WORKING_DIR" install --only main --no-root --no-interaction
    ;;
  build)
    python3 "$script_dir/run-command.py" "$workspace" "$run_dir/build.log" 480 bash "$script_dir/build-and-start-kind.sh"
    ;;
  step)
    [[ $# == 1 ]] || { usage >&2; exit 2; }
    require_owned_cluster
    case "$1" in values|render) ;; *) check_capacity ;; esac
    python3 "$script_dir/run-command.py" "$workspace" "$run_dir/$1.log" "${GEN3_STEP_MAX_SECONDS:-240}" bash "$script_dir/bootstrap-services.sh" "$1"
    ;;
  test)
    [[ $# -gt 0 ]] || { printf 'Select a test node, -k expression, or --lf; use -m gen3_workflow for the suite.\n' >&2; exit 2; }
    require_owned_cluster
    check_capacity
    mkdir -p "$WORKING_DIR/output" "$WORKING_DIR/logs" "$WORKING_DIR/allure-results"
    python3 "$script_dir/configure-tests.py" "$WORKING_DIR"
    bash "$script_dir/run-tests.sh" "$WORKING_DIR" "$run_dir" \
      uv tool run --python 3.13 --from poetry==2.2.1 poetry run pytest \
      -n 0 --maxfail=1 --max-worker-restart=0 -o timeout_method=signal -vv "$@"
    ;;
  forward)
    require_owned_cluster
    kubectl -n "$NAMESPACE" port-forward service/revproxy-service 8000:80
    ;;
  doctor)
    require_owned_cluster
    check_capacity
    ;;
  status)
    require_owned_cluster
    kubectl -n "$NAMESPACE" get pods,jobs -o wide
    ;;
  logs)
    [[ $# -gt 0 ]] || { usage >&2; exit 2; }
    require_owned_cluster
    kubectl -n "$NAMESPACE" logs "$@" --all-containers --tail=100
    ;;
  down)
    require_owned_cluster
    kind delete cluster --name "$FUNNEL_CI_KIND_CLUSTER_NAME"
    rm "$FUNNEL_CI_STATE_DIR/kind-cluster-ownership"
    ;;
esac
