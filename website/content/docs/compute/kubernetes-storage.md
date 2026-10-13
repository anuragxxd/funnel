---
title: Kubernetes storage bindings
menu:
  main:
    parent: Compute
    weight: 21
---

# Kubernetes storage bindings

`Kubernetes.Storage` binds a task workspace to a named Kubernetes volume. A binding either references an existing PVC or supplies native PVC and optional PV templates. Additional bindings can expose data volumes to the worker.

When `Storage` is absent, Funnel uses the existing `PVTemplate`, `PVCTemplate`, and Job template behavior. Storage bindings do not change TES inputs, outputs, or file transfers. In particular, mounting an S3 bucket does not cause Funnel to bypass S3 transfers or treat local writes as completed uploads.

## Use an existing claim

```yaml
Worker:
  WorkDir: /opt/funnel/funnel-work-dir
Kubernetes:
  JobsNamespace: funnel-tasks
  Storage:
    WorkspaceVolume: workspace
    Bindings:
      - Name: workspace
        ExistingClaim: shared-workspace
```

The claim must exist in `JobsNamespace` and support filesystem access from both worker and executor Pods. Funnel creates a task-specific workspace directory and leaves the claim and its PV under operator ownership. `ReadWriteMany` supports placement across nodes. `ReadWriteOnce` requires compatible Pod placement. `ReadWriteOncePod` cannot serve the two Pods and is rejected.

## Request a claim from a StorageClass

```yaml
Kubernetes:
  Storage:
    WorkspaceVolume: workspace
    Bindings:
      - Name: workspace
        Templates:
          PVC: |
            apiVersion: v1
            kind: PersistentVolumeClaim
            metadata:
              name: workspace-{{.TaskId}}
            spec:
              storageClassName: research-rwx
              accessModes: [ReadWriteMany]
              resources:
                requests:
                  storage: 20Gi
```

Funnel creates the claim in `JobsNamespace` and sets the worker Job as its owner. Kubernetes garbage collection removes the claim after the Job. The StorageClass determines provisioning and reclaim behavior. This configuration requires no GenericS3 bucket and makes no PV API calls during normal submission or cleanup.

## Supply a PV and claim pair

Set `Templates.PV` alongside `Templates.PVC` to create an explicit pair. Both fields contain native Kubernetes YAML, including CSI driver settings supplied by the deployment. Funnel fills missing `PVC.spec.volumeName` and `PV.spec.claimRef` references. Conflicting references fail before resource creation.

Templates accept `TaskId`, `Namespace`, and `DiskGb`. For compatibility with existing CSI templates, `Bucket`, `Region`, and `KmsKeyID` come from the first GenericS3 entry when present. They are empty when no entry exists. Generated PV and PVC names must contain `TaskId`; `generateName` is unsupported. Templates cannot specify owner references or block volumes.

## Add a worker data mount

```yaml
Kubernetes:
  Storage:
    WorkspaceVolume: workspace
    Bindings:
      - Name: workspace
        ExistingClaim: shared-workspace
      - Name: reference-data
        ExistingClaim: reference-datasets
        WorkerMounts:
          - |
            mountPath: /mnt/reference
            readOnly: true
```

`WorkerMounts` contains native Kubernetes `volumeMount` snippets. Funnel assigns each mount the binding's name. `subPath` can restrict a view to a directory. The same workspace binding can also provide a separate root view through `WorkerMounts`. Worker mount paths cannot overlap the task workspace or existing mounts.

Only the workspace reaches executor Pods through generated mounts. Extra worker mounts do not expose data automatically at TES input paths. A storage backend must still prepare those inputs.

`MountedInputs` maps S3 input URL prefixes to these worker mount paths. Matching reads copy files into the workspace; outputs still use the storage API. See [Mounted inputs](../../storage/mounted-inputs/).

## Job template contract

With explicit storage, Funnel renders Job templates with `NeedsPVC: false` and adds the resolved volumes and mounts afterward. Templates that already condition legacy storage on `NeedsPVC` keep their other settings. A template that hardcodes conflicting volumes or mount paths must remove them before enabling bindings.

Funnel selects the sole regular container by default. For templates with sidecars, set `Storage.WorkerContainer` and `Storage.ExecutorContainer` to the respective main container names. Funnel mounts storage and injects worker configuration only into those containers. Init containers and sidecars retain their template-defined mounts.

The worker Job name must equal `TaskId`. `Worker.WorkDir` must be absolute. Per-task plugin overrides cannot change `JobsNamespace`, because a backend reconciles one configured Jobs namespace.

`Kubernetes.WorkerImage` selects the image supplied to the worker Job template and skips server Pod discovery. When it is absent, Funnel keeps the existing image discovery behavior. Templates use `{{.Image}}` to consume that setting.

The server records a versioned plan in the Job's `funnel.calypr.org/storage-plan` annotation and supplies the same plan through `FUNNEL_KUBERNETES_STORAGE_PLAN`. The worker uses the submitted claim names, namespace, and work directory even with a static ConfigMap. Do not set that environment variable or annotation in templates.

Helm resolves deployment values before Funnel renders task variables. Charts must preserve literal Funnel expressions such as `{{.TaskId}}` through Helm rendering, as they do for existing Funnel Job templates.

## Ownership, retries, and upgrades

Funnel labels resources it creates and records the worker Job UID. Creation retries accept only matching resources with that ownership. An existing resource with a conflicting owner or specification causes submission to fail without adopting it.

Explicit PV bindings add a `funnel.calypr.org/storage-cleanup` finalizer to the worker Job. Cleanup requests deletion of owned PVs and the Job before releasing that finalizer. A failed delete request leaves the plan available for retry. Funnel preserves Kubernetes storage-protection finalizers. PVC garbage collection and the storage driver complete their normal cleanup.

Finalized Jobs let cleanup recover task-selected PV names after server configuration changes. The server still needs permission to delete those PVs. Before removing cluster-scoped permissions, finish old PV-backed tasks and clean their resources. Older legacy Jobs lack the new ownership snapshot and require the legacy cleanup configuration during migration.

Provisioning checks the worker Job before and after each resource creation. If cancellation removes the Job during creation, Funnel deletes the late-created owned PV. A process crash, an ambiguous create response, or a failed compensating deletion can leave an orphan. Recovery then requires orphan cleanup with PV scanning enabled by a PV template. An existing-claim-only configuration does not scan cluster-scoped PVs.

Rebuild Go RPC plugins with the updated config package before passing explicit bindings through them; that package registers the new source variants for gob transport. Each binding must select exactly one of `ExistingClaim` or `Templates`. Duplicate binding names are errors, including duplicates produced by configuration merges.

### Migrate contributor configuration

The canonical schema preserves `Kubernetes.ForbiddenPathPrefixes` at field 20, uses `WorkerImage` at 24 and `Storage` at 25, and adds `Config.MountedInputs` at 34. Kubernetes field numbers 21–23 and the names `PVCMode`, `StorageClassName`, `SharedPVCName`, and `S3FilesFilesystemId` are reserved. Configuration with those retired names fails parsing instead of silently selecting the wrong storage behavior.

For configurations from PR #1454, replace `PVCMode: full` with native PV and PVC templates, `PVCMode: pvc` with a PVC template containing `spec.storageClassName`, and `PVCMode: shared` with `ExistingClaim`. Keep `WorkerImage` as an independent field.

Chicago's `S3FilesFilesystemId` used field 20, which conflicts with upstream `ForbiddenPathPrefixes`. Do not decode old Chicago binary configuration with the upstream schema. Both fields use a length-delimited wire representation, so the decoder cannot distinguish them. Export that configuration using its original schema, move the filesystem ID into the deployment's native CSI PV template, and load the migrated named configuration. Rebuild plugins against the canonical schema before exchanging configuration. Funnel supplies no automatic decoder for the conflicting binary format.

## Verify against a local cluster

The opt-in integration test uses an existing Kubernetes context, creates temporary namespaces, and runs worker and executor Jobs with `alpine:3.21`. Use a disposable single-node cluster with a `standard` StorageClass and access to that image. The test checks file exchange through a provisioned claim, an existing claim, and an explicit hostPath PV/PVC pair. It then verifies ownership-based cleanup, including PV protection and Job finalizers.

```sh
FUNNEL_KUBERNETES_TEST_KUBECONFIG=/path/to/test-kubeconfig \
  go test ./compute/kubernetes -run TestStorageBindingsLive -v -count=1
```

The test verifies real Kubernetes mounts and cleanup. It does not certify an S3 CSI driver or asynchronous S3 output publication.
