---
title: Mounted inputs
menu:
  main:
    parent: Storage
---

# Mounted inputs

`MountedInputs` lets a worker read S3 inputs through an explicitly configured filesystem mount. Each mapping connects an S3 URL prefix to a directory visible to the worker. It does not create the mount or change output publication.

```yaml
MountedInputs:
  - URLPrefix: s3://analysis-data/reference/
    Path: /mnt/reference
```

With this configuration, `s3://analysis-data/reference/genome.fa` reads `/mnt/reference/genome.fa`. The worker copies the file into the task workspace through the existing input-transfer path. An executor receives the workspace copy, so changing that copy does not change the mounted source. Directory inputs list files beneath the mapped directory and preserve their relative paths.

Mappings follow Funnel's existing literal S3-key convention. Spaces, `#`, and percent signs remain part of the key; `a%20b.txt` names the literal file `a%20b.txt`, not `a b.txt`. Prefixes match whole path components: the example does not match `s3://analysis-data/reference-other/file`. Different buckets, unmatched prefixes, and other URL schemes use the configured storage backends. Overlapping mappings are configuration errors.

The local path must be absolute and clean. Matched inputs must exist beneath that root. Funnel rejects traversal and escaping symlinks through Go's rooted filesystem operations, including during file opens. Regular-file symlinks within the root can be read. Directory listings reject directory symlinks and special files. A missing or unsafe matched input fails the transfer; Funnel does not retry that input through a remote backend.

Only workers enable these mappings. The server's normal storage client does not access worker filesystem paths. Server-side storage validation and unmatched transfers still need their normal backend configuration and permissions.

## Kubernetes configuration

Use `Kubernetes.Storage.Bindings[].WorkerMounts` to expose a deployment-provided PVC at the mapped path. For example:

```yaml
Kubernetes:
  Storage:
    WorkspaceVolume: workspace
    Bindings:
      - Name: workspace
        ExistingClaim: task-workspace
      - Name: references
        ExistingClaim: reference-data
        WorkerMounts:
          - |
            mountPath: /mnt/reference
            readOnly: true
MountedInputs:
  - URLPrefix: s3://analysis-data/reference/
    Path: /mnt/reference
```

With explicit storage bindings, Funnel records the mappings in the submitted storage plan. The worker uses that plan even when a static ConfigMap contains different mappings. Deployments using legacy storage templates must supply consistent worker configuration themselves.

## Output publication

All writes through this wrapper use the configured storage backend. Even an output URL matching `MountedInputs` goes through the remote API. Funnel does not copy outputs into the input mount or wait a fixed interval for an external exporter.

The GenericS3 backend returns metadata from the successful upload response. An existing destination object, a local file, or a successful metadata lookup cannot replace that write acknowledgement. A failed upload remains a failure even if an older object exists at the destination.

Funnel uploads directory outputs as individual files. It records successful files in TES output logs, preserves partial successes when other files fail, and waits for transfers before deciding the terminal task state. Cancellation and transfer errors prevent successful task completion. Retries retain one successful log entry per file after the transfer succeeds.

Output API credentials remain required. This configuration does not provide Chicago's credential-free mounted-output workflow. Supporting that workflow requires a verified completion mechanism for the deployed storage driver, plus corresponding failure and cleanup behavior.
