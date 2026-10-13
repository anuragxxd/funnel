---
title: Mounted inputs
menu:
  main:
    parent: Storage
---

# Mounted inputs

`GenericS3.MountedRead` lets a Kubernetes worker read matching S3 inputs from a named filesystem mount. Configure the mount once under `Kubernetes.Storage.Bindings`. The GenericS3 backend then refers to it by name.

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
```

`Prefix` is relative to the bucket. An empty prefix selects the whole bucket. The mount root represents the selected prefix, so `s3://analysis-data/genomes/genome.fa` maps to `/mnt/reference/genome.fa`. Funnel does not append the prefix to `mountPath`.

The `WorkerMount` value must name a native `WorkerMounts` entry. An unknown name fails task planning. Its `Mount` contains one Kubernetes `volumeMount` YAML object with one authored `mountPath`. Kubernetes applies `subPath` or `subPathExpr` to the mounted view. Funnel uses the resulting view at `mountPath` as the read root and does not append the subpath again. The storage driver and operator must make the mounted view correspond to the configured bucket prefix and provide the required freshness.

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

The worker activates a mounted read only when the submitted storage plan names the exact GenericS3 `Endpoint` and `Bucket`. The plan stores `S3ReadMounts{Endpoint, Bucket, Prefix, Root}` without credentials. The worker ignores static `MountedRead` settings, including when the submitted plan has no mounted reads. Configured mounted reads require `Kubernetes.Storage` and its submitted plan. The API server's `NewMux` does not activate filesystem reads.

GenericS3 handles inputs whose bucket-relative key matches its configured prefix. Funnel treats S3 keys literally. Spaces, `#`, and percent signs remain part of the key. For example, `a%20b.txt` names the literal file `a%20b.txt`. Prefix matching uses whole path components, so `genomes/` does not match `genomes-old/file.fa`. Unmatched URLs use the configured storage API path.

A matched file must exist beneath the resolved mount root. Funnel rejects traversal, encoded traversal forms, and escaping symlinks through rooted filesystem operations. Regular-file symlinks within the root can be read. Directory listings reject directory symlinks and special files. A missing or unsafe matched input fails the transfer. Funnel does not fall back to the API for that input.

## Configuration ownership

The server resolves each backend's worker mount and records mounted reads in the submitted version 3 storage plan. Workers use the plan directly, even when their static ConfigMap differs. The server does not read worker filesystem paths. The version 3 decoder cannot run or clean up Jobs with version 1 or version 2 storage plans. Finish those tasks and clean up their Jobs with the older binary before upgrading.

Other compute backends retain their existing storage clients. No TES schema fields change.

## Output publication

Output writes always use the configured storage API, including URLs that match an input mount. Funnel does not copy outputs into input mounts. Output API credentials remain required.

GenericS3 returns metadata from the successful upload response. An existing destination object, a local file, or a successful metadata lookup cannot replace that write acknowledgement. A rejected upload remains a failure even if an older destination object exists.

Funnel uploads directory outputs as individual files. It records successful files in TES output logs, preserves partial successes when other files fail, and waits for transfers before deciding the terminal task state. Cancellation and transfer errors prevent successful task completion. Retries retain one successful log entry per file after the transfer succeeds.
