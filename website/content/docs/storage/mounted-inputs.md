---
title: Mounted inputs
menu:
  main:
    parent: Storage
---

# Mounted inputs

`Kubernetes.Storage.MountedInputs` lets a worker read S3 inputs through named filesystem mounts. An input mapping contains a URL prefix and a worker mount name. The mount's native `mountPath` is the only configured filesystem root.

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
          - Name: reference-root
            Mount: |
              mountPath: /mnt/reference
              subPath: release-1
              readOnly: true
    MountedInputs:
      - URLPrefix: s3://analysis-data/reference/
        WorkerMount: reference-root
```

This maps `s3://analysis-data/reference/genome.fa` to `/mnt/reference/genome.fa` in the worker. Kubernetes exposes the PVC's `release-1` directory at that path. Funnel does not append `release-1` again. Changing `mountPath` changes both the generated mount and input read root.

The mount record's `Name` identifies a worker-visible view. It is separate from the enclosing volume binding's `Name`. One PVC binding can have several named mounts with different native `subPath`, `subPathExpr`, and access settings. Input mappings select a view by name, independently of list order. Names must be unique across the storage configuration. Unknown references and overlapping URL prefixes fail task planning before resource creation.

## TES behavior

Clients continue to submit ordinary TES inputs. They do not supply PVC names, worker mount names, or deployment paths.

```json
{
  "inputs": [
    {
      "url": "s3://analysis-data/reference/genome.fa",
      "path": "/inputs/genome.fa",
      "type": "FILE"
    }
  ],
  "executors": [
    {
      "image": "alpine:3.21",
      "command": ["cat", "/inputs/genome.fa"]
    }
  ]
}
```

The worker copies the source into the task workspace through the existing input-transfer path. The executor sees the copy at `/inputs/genome.fa`. Changing that copy does not change the mounted source. Directory inputs preserve their relative filenames. Reference-data mounts remain worker-only; they do not turn TES `volumes` into pre-populated directories. TES volumes retain their existing empty scratch-directory behavior.

Mappings follow Funnel's literal S3-key convention. Spaces, `#`, and percent signs remain part of the key. For example, `a%20b.txt` names the literal file `a%20b.txt`. Prefixes match whole path components, so the example does not match `s3://analysis-data/reference-other/file`. Unmatched inputs use the configured storage backends.

Matched files must exist beneath the resolved mount root. Funnel rejects traversal, encoded traversal forms, and escaping symlinks through rooted filesystem operations. Regular-file symlinks within the root can be read. Directory listings reject directory symlinks and special files. A missing or unsafe matched input fails the transfer instead of falling back to remote storage.

The deployment must ensure that the mounted view represents the declared S3 prefix and provides the required consistency. Mount configuration alone cannot prove object freshness.

## Configuration ownership

The server resolves mount references once and records native mounts and derived input routes in the submitted storage plan. Workers consume that snapshot directly, even when their static ConfigMap differs. A decoded route must identify a worker mount in the same plan. The server's normal storage client does not access worker filesystem paths.

The former top-level `MountedInputs` path configuration is removed from this unmerged draft. Kubernetes input mappings require explicit storage bindings and a submitted plan. Other compute backends retain their existing storage clients. No TES schema fields change.

## Output publication

Output writes always use the configured storage backend, including URLs that match an input mapping. Funnel does not copy outputs into input mounts or wait a fixed interval for an external exporter. Mounted-read validation does not restrict the backend's output URL support.

GenericS3 returns metadata from the successful upload response. An existing destination object, a local file, or a successful metadata lookup cannot replace that write acknowledgement. A rejected upload remains a failure even if an older destination object exists.

Funnel uploads directory outputs as individual files. It records successful files in TES output logs, preserves partial successes when other files fail, and waits for transfers before deciding the terminal task state. Cancellation and transfer errors prevent successful task completion. Retries retain one successful log entry per file after the transfer succeeds.

Output API credentials remain required. Mounted-output publication requires a verified completion mechanism for the deployed storage driver and remains outside this implementation.
