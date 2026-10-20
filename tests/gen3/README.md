The Gen3 check has a ten-minute job limit and a five-to-six-minute runtime
target. The budget includes dependency setup, image builds, cluster startup,
service preparation, tests, and diagnostics. Hosted run timings establish whether
the target is met. A shorter timeout alone is not a performance improvement.

The repository owns the workflow and lifecycle runner. The runner uses pinned
integration-test and GitOps revisions, prepares dependencies alongside the
candidate build and two-node Kind cluster, and runs the upstream
`gen3_workflow and not wip` test selection. The fixture uses one replica of each
required Gen3 service and omits the unused Elasticsearch pod. It retains the
real authentication services, gateway, PostgreSQL, MinIO, and Mountpoint CSI.

The Go compiler and module caches persist across runs. The candidate image is
built from the checked-out Funnel source. Its identity is checked on both Kind
nodes and against the running server. Workers inherit that image with registry
pulls disabled. The Helm post-renderer prevents the chart's default Funnel image
from replacing the candidate.

The authorizer is built from the pinned
[`funnel-gen3-plugin` commit `eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3`](https://github.com/calypr/funnel-gen3-plugin/commit/eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3)
and the candidate's Funnel module. Both Kind nodes receive that image, and the
verification hook checks the running plugin init container. The renderer's
plugin override remains optional for other callers.

The pytest controller creates test API keys once before starting its workers.
TES completion polling uses two-second intervals and a 90-attempt limit, with
the existing completion and output assertions unchanged.

Tests stream their output to the job log and artifacts. Diagnostics cover the
Gen3 service namespace and the separate worker namespace. The workflow reserves
time for a final snapshot and uploads artifacts after failure. Inspect the
phase timings, pytest output, pod states, and image identity when a check fails.

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
workflows. Verify anonymous mirror pulls before updating the references. The
workflow uses the setup scripts and image values from its own checkout.
