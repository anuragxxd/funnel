The Gen3 check builds the checked-out commit from a `git archive` snapshot while
starting a two-node Kind cluster. It imports that image into both nodes and uses
a Helm post-renderer to select it for the Funnel server and workers with pulls
disabled. Before testing, it checks the running server image against the imported
image identity and records the result in `logs/gen3-image-identity.json`.

This avoids waiting for the separate multi-architecture Quay publishing job.
The upstream Helm helper writes an unused nested image override for Funnel, so
the post-renderer also prevents accidentally testing `cdis/funnel:main-gen3`.
The build, cluster setup, and import durations are printed separately; the total
CI improvement must be measured on hosted runs.

The Gen3 authorizer plugin is built from the pinned
[`funnel-gen3-plugin` commit `eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3`](https://github.com/calypr/funnel-gen3-plugin/commit/eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3).
CI uses the same Funnel builder stage and module source as the candidate image,
then imports the plugin image into both Kind nodes. The post-renderer selects it
for the `plugin` init container with pulls disabled, and the setup check records
the plugin source commit and verifies the running init-container image ID.
The plugin override stays optional for other users of this renderer.

The upstream test suite still uses MinIO and the real Mountpoint S3 CSI driver.
The setup script pins CSI chart
2.8.0 and uses Linux/amd64 images mirrored in `quay.io/ohsu-comp-bio/funnel`
under `ci-csi-*` tags. GitHub-hosted runners exhausted ECR's anonymous data
allowance, so pulling directly from ECR failed before tests could start.

Each mirror preserves the corresponding upstream Linux/amd64 manifest digest.
The Helm image references include that digest, so a changed tag cannot substitute
different image contents.

| Upstream image | Linux/amd64 manifest digest |
| --- | --- |
| `public.ecr.aws/mountpoint-s3-csi-driver/aws-mountpoint-s3-csi-driver:v2.8.0` | `sha256:484def4a3b2eec778ebae1c1574e2b8685df68d316d489fb7dc8d8bef814cb84` |
| `public.ecr.aws/csi-components/csi-node-driver-registrar:v2.17.0-eksbuild.5` | `sha256:aa84e8d445a27744f21d4eb90dc2810e681e1fe4681f0ec60dd0db9454d4aef2` |
| `public.ecr.aws/csi-components/livenessprobe:v2.19.0-eksbuild.5` | `sha256:6bc3e10863a825c6d5c48492bce8c4be4a6582aa77289975bccd17ab164590ed` |

When upgrading, copy the selected upstream manifests with `crane copy`, verify
that `crane digest` returns the same digest for each mirror, and update the chart
version and image references together in `csi-images.yaml` and both Kubernetes
workflows. Pin `setup-environment.sh` to the commit containing those values.
Verify anonymous mirror pulls before updating the references. The workflow loads
the setup scripts from its own commit so the tested scripts and source agree.
