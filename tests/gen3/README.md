The Gen3 check runs the upstream test suite against a two-node Kind cluster,
MinIO, and the real Mountpoint S3 CSI driver. Its setup script pins CSI chart
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
version and image references together in `setup-environment.sh`. Verify anonymous
mirror pulls before changing the workflow's pinned setup-script commit.
