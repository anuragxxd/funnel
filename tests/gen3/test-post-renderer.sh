#!/usr/bin/env bash
set -Eeuo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/input.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: funnel-server
spec:
  template:
    spec:
      initContainers:
        - name: config-updater
          image: quay.io/cdis/awshelper:master
        - name: plugin
          image: example/plugin:old
      containers:
        - name: funnel
          image: example/funnel:old
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: funnel-server-config
data:
  funnel-server.yaml: |
    Kubernetes:
      WorkerTemplate: |
        containers:
          - image: {{.Image}}
            imagePullPolicy: Always
YAML
export FUNNEL_CI_IMAGE_REF=quay.io/ohsu-comp-bio/funnel:ci-123456789abc
registry_helper=$(yq -r '.global.awshelper_container_image' "$script_dir/values-ci.yaml")
for mode in plain gen3; do
  export FUNNEL_CI_PLUGIN_IMAGE_REF=
  if [[ "$mode" == gen3 ]]; then
    export FUNNEL_CI_PLUGIN_IMAGE_REF=quay.io/ohsu-comp-bio/funnel-gen3-plugin:ci-123456789abc-eacaf197bfbc
  fi
  FUNNEL_CI_BOOTSTRAP_RENDER=false bash "$script_dir/gen3-post-renderer.sh" <"$tmp/input.yaml" >"$tmp/$mode.yaml"
  helper=$(yq -r 'select(.kind == "Deployment") | .spec.template.spec.initContainers[] | select(.name == "config-updater") | .image' "$tmp/$mode.yaml")
  if [[ "$mode" == plain ]]; then
    [[ "$helper" == "$registry_helper" ]] || { printf 'Plain chart selected a Gen3-only local helper\n' >&2; exit 1; }
  else
    [[ "$helper" == funnel-gen3-awshelper:ci ]] || { printf 'Gen3 chart did not select its native helper\n' >&2; exit 1; }
    policy=$(yq -r 'select(.kind == "Deployment") | .spec.template.spec.initContainers[] | select(.name == "config-updater") | .imagePullPolicy' "$tmp/$mode.yaml")
    [[ "$policy" == Never ]]
  fi
  printf 'PASS: %s chart selects the correct helper image\n' "$mode"
done
