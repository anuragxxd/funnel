#!/usr/bin/env bash
set -Eeuo pipefail

: "${FUNNEL_CI_IMAGE_REF:?candidate image reference is required}"
if [[ ! "$FUNNEL_CI_IMAGE_REF" =~ ^quay\.io/ohsu-comp-bio/funnel:ci-[0-9a-f]{12}$ ]]; then
  printf 'unexpected candidate image reference: %s\n' "$FUNNEL_CI_IMAGE_REF" >&2
  exit 1
fi

render_dir=$(mktemp -d)
trap 'rm -rf "$render_dir"' EXIT
input="$render_dir/input.yaml"
output="$render_dir/output.yaml"
cat >"$input"

deployment_count=$(yq eval-all -r '[select(.kind == "Deployment" and .metadata.name == "funnel-server")] | length' "$input")
configmap_count=$(yq eval-all -r '[select(.kind == "ConfigMap" and .metadata.name == "funnel-server-config")] | length' "$input")
if [[ "$deployment_count" != "1" || "$configmap_count" != "1" ]]; then
  printf 'expected one Funnel server Deployment and config map, got %s and %s\n' "$deployment_count" "$configmap_count" >&2
  exit 1
fi

worker_template=$(yq eval-all -r 'select(.kind == "ConfigMap" and .metadata.name == "funnel-server-config") | .data."funnel-server.yaml" | from_yaml | .Kubernetes.WorkerTemplate' "$input")
if [[ $(printf '%s\n' "$worker_template" | awk '/imagePullPolicy: Always$/ { count++ } END { print count + 0 }') != "1" ]]; then
  printf 'expected one Always policy in the rendered Funnel worker template\n' >&2
  exit 1
fi
if [[ "$worker_template" != *'image: {{.Image}}'* ]]; then
  printf 'Funnel worker template no longer uses the server image reference\n' >&2
  exit 1
fi

yq eval '
  (
    (select(.kind == "Deployment" and .metadata.name == "funnel-server") |
      (.spec.template.spec.containers[] | select(.name == "funnel")) |=
        (.image = strenv(FUNNEL_CI_IMAGE_REF) | .imagePullPolicy = "Never")
    ) //
    (select(.kind == "ConfigMap" and .metadata.name == "funnel-server-config") |
      .data."funnel-server.yaml" |= (
        from_yaml |
        .Kubernetes.WorkerTemplate |= sub("imagePullPolicy: Always"; "imagePullPolicy: Never") |
        to_yaml
      )
    ) // .
  )
' "$input" >"$output"

actual_image=$(yq eval-all -r 'select(.kind == "Deployment" and .metadata.name == "funnel-server") | .spec.template.spec.containers[] | select(.name == "funnel") | .image' "$output")
actual_server_policy=$(yq eval-all -r 'select(.kind == "Deployment" and .metadata.name == "funnel-server") | .spec.template.spec.containers[] | select(.name == "funnel") | .imagePullPolicy' "$output")
worker_template=$(yq eval-all -r 'select(.kind == "ConfigMap" and .metadata.name == "funnel-server-config") | .data."funnel-server.yaml" | from_yaml | .Kubernetes.WorkerTemplate' "$output")
if [[ "$actual_image" != "$FUNNEL_CI_IMAGE_REF" || "$actual_server_policy" != "Never" ]]; then
  printf 'post-rendered server image/policy mismatch: %s %s\n' "$actual_image" "$actual_server_policy" >&2
  exit 1
fi
if [[ "$worker_template" != *'image: {{.Image}}'* || "$worker_template" != *'imagePullPolicy: Never'* || "$worker_template" == *'imagePullPolicy: Always'* ]]; then
  printf 'post-rendered Funnel worker template does not use the loaded server image without pulling\n' >&2
  exit 1
fi

printf 'rendered local Funnel image %s for server and worker\n' "$FUNNEL_CI_IMAGE_REF" >&2
cat "$output"
