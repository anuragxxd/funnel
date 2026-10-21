For local debugging, keep the environment between edits. `dev.sh` stores its
kubeconfig, dependency caches, and per-command logs in ignored `.gen3-local/`.
It only accesses the cluster recorded as owned by this checkout. Local runs use
the fixture’s MinIO credentials and ignore host AWS profiles.

Prerequisites: Docker, Kind 0.31.0, kubectl, Helm 4.3.1, yq 4, jq, AWS CLI,
Python 3.13, uv, and GNU `timeout`. Put pinned tools in `.gen3-local/bin/`
or on `PATH`. Leave room in Docker’s virtual disk for service and executor
images. Local test and bootstrap steps require at least 2 GiB free there; host
filesystem free space alone does not establish Docker capacity.

```sh
bash tests/gen3/dev.sh deps                  # Once: pinned test dependencies
bash tests/gen3/dev.sh build                 # Once, then only for image changes
bash tests/gen3/dev.sh step prepare          # Once: database, auth, storage
bash tests/gen3/dev.sh step deploy
bash tests/gen3/dev.sh forward               # Leave running in another terminal
bash tests/gen3/dev.sh test -k directory      # Rerun a focused test
```

After a bootstrap failure, edit the script and rerun the failing phase with
`step database`, `step auth`, `step users`, or `step client`. These commands do
not rebuild images or recreate Kind. `step values` checks configuration without
deploying it. `status` and `logs deployment/fence-deployment` inspect retained
resources. Cleanup happens only with the explicit `down` command.

After a normal test run has initialized authentication fixtures, use
`RUNNING_LOCAL=true bash tests/gen3/dev.sh test -k directory` for focused
retries that reuse those fixtures. Run the complete selection without
`RUNNING_LOCAL` for final verification.

Local image builds include uncommitted source and use content-derived image tags.
A local run on an ARM host builds ARM images; CI still builds AMD64. Focused local
passes are debugging evidence; the complete fresh CI run remains the final check.

The Gen3 check has a ten-minute job limit and a five-to-six-minute runtime
target. The budget includes dependency setup, image builds, cluster startup,
service preparation, tests, and diagnostics. Hosted run timings establish whether
the target is met. A shorter timeout alone is not a performance improvement.

The repository owns the workflow and lifecycle runner. The runner uses pinned
integration-test and GitOps revisions, prepares dependencies alongside the
candidate build and two-node Kind cluster. Once Kind is ready, database/auth
bootstrap and storage setup run concurrently with compilation. The runner uses
bounded parallel image pulls and runs the upstream
`gen3_workflow and not wip` test selection. The fixture uses one replica of each
required Gen3 service and omits the unused Elasticsearch pod. It retains the
real authentication services, PostgreSQL, MinIO, and Mountpoint CSI. Two
gateways share the same Funnel backend: `/workflows/` keeps the upstream Kind
bearer-token mode, while `/ga4gh/tes/` validates real DPoP proofs. This keeps the
task-token executor test enabled without changing ordinary client authentication.
Before pytest starts, a request with an invalid proof must return HTTP 401 with
`invalid_dpop_proof`; a disabled or misrouted gateway fails setup immediately.
Use `step dpop` to refresh those routes in a retained environment, then restart
your local port-forward.
The chart’s network policies isolate executors from internal Gen3 services;
fixture policies permit the required MinIO traffic and executor access to the
public Gen3 gateway. Fence advertises that gateway in API-key issuer URLs so
task-token clients can reach both authentication and workflow endpoints. `user.yaml` preserves the
pinned upstream Kind fixture, including read-only access to the main user’s
tasks for the delegated-access test. It also grants the main user `create` on
`/services/fence/task-token/WORKFLOW` for the task-token integration case.
MinIO stores the small test dataset on a 1 GiB memory-backed volume, keeping its
free-space checks independent of the Docker host’s shared disk usage.

The Go compiler and module caches persist across runs. The candidate image is
built from the checked-out Funnel source. Its identity is checked on both Kind
nodes and against the running server. Workers inherit that image with registry
pulls disabled. The Helm post-renderer prevents the chart's default Funnel image
from replacing the candidate.

The authorizer is built from the pinned
[`funnel-gen3-plugin` commit `eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3`](https://github.com/calypr/funnel-gen3-plugin/commit/eacaf197bfbc786c753d9ecfb86a2a2f7022a5e3)
and the candidate's Funnel module. Both Kind nodes receive that image, and the
verification hook checks the running plugin init container. The renderer requires
both candidate image references to prevent a registry image from replacing either
built artifact.

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
