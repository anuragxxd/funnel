---
title: Mounted inputs
menu:
  main:
    parent: Storage
---

# Mounted inputs

`GenericS3.MountedRead` and `GenericS3.MountedWrite` let a Kubernetes worker read inputs or publish outputs through named filesystem mounts. Configure each mount under `Kubernetes.Storage.Bindings`. The GenericS3 backend refers to each mount by name. Reads and writes are optional and can use separate mounts.

```yaml
GenericS3:
  - Disabled: false
    Endpoint: https://s3.example.org
    Bucket: analysis-data
    Key: storage-key
    Secret: storage-secret
    Region: us-east-1
    MountedRead:
      WorkerMount: reference-root
      Prefix: genomes/
    MountedWrite:
      WorkerMount: results-root
      Prefix: results/
      Timeout: 45m

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
              readOnly: true
      - Name: results
        ExistingClaim: task-results
        WorkerMounts:
          - Name: results-root
            Mount: |
              mountPath: /mnt/results
```

`Prefix` is relative to the bucket. An empty prefix selects the whole bucket. The mount root represents the selected prefix, so `s3://analysis-data/genomes/genome.fa` maps to `/mnt/reference/genome.fa`. Funnel does not append the prefix to `mountPath`.

The `WorkerMount` value must name a native `WorkerMounts` entry. An unknown name fails task planning. Its `Mount` contains one Kubernetes `volumeMount` YAML object with one authored `mountPath`. Kubernetes applies `subPath` or `subPathExpr` to the mounted view. Funnel uses the resulting view at `mountPath` as the read root and does not append the subpath again. The storage driver and operator must make the mounted view correspond to the configured bucket prefix and provide the required freshness.

`MountedWrite` uses the same naming and prefix rules. Its worker mount must be writable, including the underlying PVC volume. The mounted view represents the configured bucket prefix. Funnel appends the output key path relative to that prefix when it copies a TES output file into the mount.

## TES behavior

Clients continue to submit ordinary TES inputs. They do not supply PVC names, worker mount names, or deployment paths.

```json
{
  "inputs": [
    {
      "url": "s3://analysis-data/genomes/genome.fa",
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

The worker copies each input into the task workspace. For a matching GenericS3 mounted read, it reads the source from the filesystem mount. The executor sees the copy at `/inputs/genome.fa`. Changing that copy does not change the mounted source. Directory inputs preserve their relative filenames. Reference mounts remain worker-only. They do not change TES `volumes`, which retain their empty scratch-directory behavior.

## Read routing and safety

The worker activates a mounted read or write only when the submitted storage plan names the exact GenericS3 `Endpoint` and `Bucket`. The plan stores `S3ReadMounts` and `S3WriteMounts` with endpoint, bucket, prefix, and root, without credentials. The worker ignores static `MountedRead` and `MountedWrite` settings, including when the submitted plan has no mounted routes. Configured mounted reads and writes require `Kubernetes.Storage` and its submitted plan. The API server's `NewMux` does not activate filesystem reads or writes.

GenericS3 handles inputs whose bucket-relative key matches its configured prefix. Funnel treats S3 keys literally. Spaces, `#`, and percent signs remain part of the key. For example, `a%20b.txt` names the literal file `a%20b.txt`. Prefix matching uses whole path components, so `genomes/` does not match `genomes-old/file.fa`. Unmatched URLs use the configured storage API path.

A matched file must exist beneath the resolved mount root. Funnel rejects traversal, encoded traversal forms, and escaping symlinks through rooted filesystem operations. Regular-file symlinks within the root can be read. Directory listings reject directory symlinks and special files. A missing or unsafe matched input fails the transfer. Funnel does not fall back to the API for that input.

## Configuration ownership

The server resolves each backend's worker mounts and records mounted reads and writes in version 4 storage plans. Workers use the plan directly, even when their static ConfigMap differs. The server does not read worker filesystem paths. The version 4 decoder cannot run or clean up Jobs with version 1, 2, or 3 storage plans. Finish those tasks and clean up their Jobs with a compatible older binary before upgrading.

Other compute backends retain their existing storage clients. No TES schema fields change.

## Output publication

For an output URL that matches a configured `MountedWrite` endpoint, bucket, and prefix, the worker copies each output file into the named mount. It then calls S3 `HeadObject` until the object exists with the same size as the local file. The worker checks zero-byte files too.

This check proves that S3 reports an object with the expected size. It does not compare file contents, checksums, or object versions. A same-sized object left by an earlier task can satisfy the check. Set `MountedWrite.Timeout` to bound polling. The default is three hours, and the worker polls every two seconds. An explicit timeout must be positive. Task cancellation also stops the wait.

The GenericS3 credentials must allow `HeadObject` for mounted destinations. They do not need upload permission for an output handled by the mount. An output URL that does not match the configured endpoint, bucket, and prefix follows the regular GenericS3 API upload path, which requires upload permission. Outputs do not use a `MountedRead` setting.

Funnel handles directory outputs as individual files. It records successful files in TES output logs and preserves partial successes when another file fails. A timeout, cancellation, or transfer error prevents successful task completion. Retries retain one successful log entry per file after publication succeeds.
