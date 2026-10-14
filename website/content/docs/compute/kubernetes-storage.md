---
title: Kubernetes storage
menu:
  main:
    parent: Compute
    weight: 21
---

# Kubernetes storage

`Kubernetes.Storage` configures PVC-backed Kubernetes volumes for task workspaces and worker mounts. The `volumes` and `volumeMounts` fields use native Kubernetes objects, and configured volumes must use PVC sources. The `volumeClaimTemplates` and `persistentVolumeTemplates` fields contain native resource objects. `WorkspaceVolume` names the PVC-backed volume Funnel uses for the task workspace. `WorkerContainer` and `ExecutorContainer` select the containers that receive generated mounts when a Pod template has more than one regular container.

When `Storage` is absent, Funnel keeps the existing `PVTemplate`, `PVCTemplate`, and Job template behavior. Explicit storage does not change TES inputs, outputs, or task paths. Configured GenericS3 mounted reads and writes require `Kubernetes.Storage` and a submitted storage plan.

## Use an existing PVC

Use `persistentVolumeClaim.claimName` for a claim that an operator already manages. Funnel does not create or delete that claim or its PV.

```yaml
Worker:
  WorkDir: /opt/funnel/funnel-work-dir
Kubernetes:
  JobsNamespace: funnel-tasks
  Storage:
    WorkspaceVolume: workspace
    volumes:
      - name: workspace
        persistentVolumeClaim:
          claimName: shared-workspace
```

The claim must exist in `JobsNamespace` and support filesystem access from the worker and executor Pods. Funnel creates a task-specific workspace directory on that claim. `ReadWriteMany` supports placement across nodes. `ReadWriteOnce` requires compatible Pod placement. `ReadWriteOncePod` cannot serve the worker and executor Pods and is rejected.

Every configured volume must use a PVC. For an existing claim, set `persistentVolumeClaim.claimName` to its literal name. For a PVC template, use a Go template in that field that renders to the template's full name. A `volumeMounts` entry refers to a configured volume through its `name` field.

## Provision a PVC through a StorageClass

Use `persistentVolumeClaim.claimName` on a configured volume to refer to a PVC template. The value must match the template's full `metadata.name`, including the task ID.

```yaml
Kubernetes:
  JobsNamespace: funnel-tasks
  Storage:
    WorkspaceVolume: workspace
    volumes:
      - name: workspace
        persistentVolumeClaim:
          claimName: work-{{.TaskId}}
    volumeClaimTemplates:
      - apiVersion: v1
        kind: PersistentVolumeClaim
        metadata:
          name: work-{{.TaskId}}
        spec:
          storageClassName: research-rwx
          accessModes:
            - ReadWriteMany
          resources:
            requests:
              storage: "{{.DiskGb}}Gi"
```

Funnel creates the claim in `JobsNamespace` and sets the worker Job as its owner. Kubernetes garbage collection removes the claim after the Job. The StorageClass determines provisioning and reclaim behavior. This configuration makes no PV API calls during normal submission or cleanup.

Funnel requires each PVC template to be a `v1` `PersistentVolumeClaim` with an explicit `metadata.name` that includes `TaskId`. It infers an omitted `metadata.namespace` from the effective job namespace, which uses `JobsNamespace` or falls back to `Namespace`. Do not set `generateName`, owner references, or block volume mode. Every template must be referenced by a configured volume. Funnel does not infer a claim name from a volume name or discover a template by matching a partial name.

Funnel renders Go templates in string values, then decodes each object as a Kubernetes resource. It does not render an entire YAML document as template text. Quote templated quantity values so YAML keeps them as strings. Templates can use `.TaskId`, `.Namespace`, and `.DiskGb`. They can also use `.Bucket`, `.Region`, and `.KmsKeyID` from the first GenericS3 entry when present. Those three values are empty when no GenericS3 entry exists.

## Create an explicit PV and PVC pair

Set `PVC.spec.volumeName` to the exact `metadata.name` of a `persistentVolumeTemplates` entry. Funnel fills a missing PV `spec.claimRef` with that PVC's name and namespace. If the template provides a conflicting reference, Funnel rejects it before creating resources.

```yaml
Kubernetes:
  JobsNamespace: funnel-tasks
  Storage:
    WorkspaceVolume: workspace
    volumes:
      - name: workspace
        persistentVolumeClaim:
          claimName: data-{{.TaskId}}
    volumeClaimTemplates:
      - apiVersion: v1
        kind: PersistentVolumeClaim
        metadata:
          name: data-{{.TaskId}}
        spec:
          storageClassName: ""
          volumeName: disk-{{.TaskId}}
          accessModes:
            - ReadWriteOnce
          resources:
            requests:
              storage: 20Gi
    persistentVolumeTemplates:
      - apiVersion: v1
        kind: PersistentVolume
        metadata:
          name: disk-{{.TaskId}}
        spec:
          capacity:
            storage: 20Gi
          accessModes:
            - ReadWriteOnce
          persistentVolumeReclaimPolicy: Retain
          storageClassName: ""
          hostPath:
            path: /srv/funnel/{{.TaskId}}
```

The `hostPath` source makes this example suitable only for a local cluster whose worker and executor Pods use the same node and path. Use the deployment's native CSI or other PV source in a real cluster. Each PV template must be a cluster-scoped `v1` `PersistentVolume` with an explicit `metadata.name` that includes `TaskId`. Do not set `metadata.namespace`, `generateName`, owner references, or block volume mode. A PV template must have exactly one native PV source and link to exactly one PVC template.

Funnel creates the PV and PVC with task ownership and preserves the existing cleanup lifecycle. Explicit PV storage adds a finalizer to the worker Job. Cleanup requests deletion of owned PVs and the Job before releasing that finalizer. A failed delete request leaves the plan available for retry. The server needs permission to delete these PVs. Finish PV-backed tasks and clean up their resources before removing cluster-scoped deletion permissions. Kubernetes storage-protection finalizers and the storage driver's normal cleanup still apply.

## Add worker mounts

`volumeMounts` contains native Kubernetes `volumeMount` objects. Use the configured volume's `name`. You can mount the same volume at multiple paths.

```yaml
Kubernetes:
  JobsNamespace: funnel-tasks
  Storage:
    WorkspaceVolume: workspace
    volumes:
      - name: workspace
        persistentVolumeClaim:
          claimName: shared-workspace
      - name: reference-data
        persistentVolumeClaim:
          claimName: reference-datasets
    volumeMounts:
      - name: reference-data
        mountPath: /mnt/reference
        readOnly: true
        subPath: genomes
      - name: reference-data
        mountPath: /mnt/annotations
        readOnly: true
        subPath: annotations
```

Funnel adds `volumeMounts` to the selected worker container. It generates the workspace mount for the worker and executor. Extra worker mounts do not expose data automatically at TES input paths. A storage backend must still prepare those inputs. Worker mount paths cannot overlap the task workspace, each other, or mounts already in the Job template.

GenericS3 `MountedRead` and `MountedWrite` select a worker mount by volume name and, optionally, its static `subPath`. If one `volumeMounts` entry uses the selected volume, omit `SubPath`. If several entries use that volume, set `SubPath` to the exact `volumeMounts[].subPath` value. Set `SubPath: ""` to select a static root mount. A mount that uses `subPathExpr` can be selected only when it is the unique mount for that volume and `SubPath` is absent.

The selected mount's authored `mountPath` is the S3 root. Funnel does not append the selected `SubPath` or the S3 `Prefix` to that path. A write mount must be writable. The underlying claim must also permit writes.

## Configure Job templates

With explicit storage, Funnel renders Job templates with `NeedsPVC: false` and adds resolved volumes and mounts afterward. Templates that already condition legacy storage on `NeedsPVC` keep their other settings. Remove template-defined volumes or mount paths that conflict with configured storage.

Funnel selects the sole regular container by default. For templates with sidecars, set `Storage.WorkerContainer` and `Storage.ExecutorContainer` to the main container names. Funnel adds storage and worker configuration only to those containers. Init containers and sidecars keep their template-defined mounts.

The worker Job name must equal `TaskId`. `Worker.WorkDir` must be absolute. Per-task plugin overrides cannot change `JobsNamespace`, because a backend reconciles one configured Jobs namespace.

`Kubernetes.WorkerImage` selects the image supplied to the worker Job template and skips server Pod discovery. When it is absent, Funnel keeps the existing image discovery behavior. Templates use `{{.Image}}` to consume that setting.

## Ownership and upgrades

Funnel labels resources it creates and records the worker Job UID. Creation retries accept only resources with matching ownership and specifications. Funnel rejects an existing resource with a conflicting owner or specification instead of adopting it. Operator-owned claims remain under operator control.

Provisioning checks the worker Job before and after each resource creation. If cancellation removes the Job during creation, Funnel deletes a late-created owned PV. A process crash, an ambiguous create response, or a failed compensating deletion can leave an orphan. Recovery requires an orphan cleanup sweep with PV scanning enabled by a PV template. An existing-claim-only configuration does not scan cluster-scoped PVs.

Funnel records a version 4 plan in the Job's `funnel.calypr.org/storage-plan` annotation and supplies the same plan through `FUNNEL_KUBERNETES_STORAGE_PLAN`. The plan records resolved volumes, worker mounts, S3 routes, claim names, namespace, and work directory. It contains no credentials. Existing Jobs with version 4 plans remain compatible. Do not set the plan environment variable or annotation in templates.

The config schema changed. Migrate configurations to native objects and rebuild Go RPC plugins against the updated config package before exchanging configuration with them. The schema preserves `Kubernetes.ForbiddenPathPrefixes` at field 20 and uses `WorkerImage` at 24 and `Storage` at 25. It reserves `KubernetesStorage` field 2 and the names `Bindings` and `MountedInputs`. `MountedRead` and `MountedWrite` reserve field 1 and the name `WorkerMount`. Kubernetes field numbers 21–23 and the names `PVCMode`, `StorageClassName`, `SharedPVCName`, and `S3FilesFilesystemId` are also reserved. Configuration with retired names fails parsing.

For configurations from PR #1454, replace `PVCMode: full` with native PV and PVC templates, `PVCMode: pvc` with a PVC template containing `spec.storageClassName`, and `PVCMode: shared` with `persistentVolumeClaim.claimName`. Keep `WorkerImage` as an independent field.

Chicago's `S3FilesFilesystemId` used field 20, which conflicts with upstream `ForbiddenPathPrefixes`. Do not decode old Chicago binary configuration with the upstream schema. Both fields use a length-delimited wire representation, so a decoder cannot distinguish them. Export the configuration with its original schema, move the filesystem ID into the deployment's native CSI PV template, and load the migrated named configuration. Rebuild plugins against the canonical schema before exchanging configuration. Funnel supplies no automatic decoder for the conflicting binary format.

The version 4 decoder cannot run or clean up Jobs with version 1, 2, or 3 plans. Finish those tasks and clean up their Jobs with a compatible older binary before upgrading. Jobs without a storage plan still require the legacy cleanup configuration during migration. These requirements apply whether the task uses existing claims or native PVC templates.

Helm resolves deployment values before Funnel renders task variables. Charts must preserve literal Funnel expressions such as `{{.TaskId}}` through Helm rendering, as they do for existing Funnel Job templates.

## Verify against a local cluster

The opt-in integration test uses an existing Kubernetes context, creates temporary namespaces, and runs worker and executor Jobs with `alpine:3.21`. Use a disposable single-node cluster with a `standard` StorageClass and access to that image. The test checks file exchange through a provisioned claim, an existing claim, and an explicit hostPath PV and PVC pair. It then verifies ownership-based cleanup, including PV protection and Job finalizers.

```sh
FUNNEL_KUBERNETES_TEST_KUBECONFIG=/path/to/test-kubeconfig \
  go test ./compute/kubernetes -run TestStorageBindingsLive -v -count=1
```

The test verifies real Kubernetes mounts and cleanup. It does not certify an S3 CSI driver or asynchronous S3 output publication.
