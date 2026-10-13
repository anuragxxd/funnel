package storageplan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ohsu-comp-bio/funnel/config"
	"github.com/ohsu-comp-bio/funnel/storage"
	"github.com/ohsu-comp-bio/funnel/tes"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func existingConfig() *config.Config {
	c := config.DefaultConfig()
	c.Kubernetes.JobsNamespace = "jobs"
	c.Worker.WorkDir = "/work"
	c.Kubernetes.Storage = &config.KubernetesStorage{WorkspaceVolume: "workspace", Bindings: []*config.KubernetesStorageBinding{{Name: "workspace", Source: &config.KubernetesStorageBinding_ExistingClaim{ExistingClaim: "shared-data"}}}}
	return c
}

func namedMount(name, raw string) *config.KubernetesWorkerMount {
	return &config.KubernetesWorkerMount{Name: name, Mount: raw}
}

func mountedS3(bucket, prefix, workerMount string) *config.GenericS3Storage {
	return &config.GenericS3Storage{
		Endpoint: "https://s3.example.test/custom/path",
		Key:      "access-key",
		Secret:   "access-secret",
		Bucket:   bucket,
		MountedRead: &config.S3MountedRead{
			WorkerMount: workerMount,
			Prefix:      prefix,
		},
	}
}

const pvcTemplate = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: claim-{{.TaskId}}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: fast
  resources:
    requests:
      storage: 5Gi
`

const pvTemplate = `apiVersion: v1
kind: PersistentVolume
metadata:
  name: disk-{{.TaskId}}
spec:
  capacity:
    storage: 5Gi
  accessModes: [ReadWriteMany]
  storageClassName: fast
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: example.csi
    volumeHandle: handle-{{.TaskId}}
    volumeAttributes:
      bucket: native-bucket
`

func TestExistingClaimWorkerExecutorSnapshot(t *testing.T) {
	c := existingConfig()
	c.Kubernetes.Storage.Bindings = append(c.Kubernetes.Storage.Bindings, &config.KubernetesStorageBinding{Name: "cache", Source: &config.KubernetesStorageBinding_ExistingClaim{ExistingClaim: "shared-cache"}, WorkerMounts: []*config.KubernetesWorkerMount{namedMount("cache-view", "mountPath: /cache\nreadOnly: true\nsubPath: cache")}})
	p, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.OwnedClaims) != 0 || len(p.OwnedVolumes) != 0 {
		t.Fatalf("external claims became owned: %+v", p)
	}
	worker := testJob()
	if err := p.ApplyWorker(worker, "/work", "task-123"); err != nil {
		t.Fatal(err)
	}
	if len(worker.Spec.Template.Spec.Volumes) != 2 {
		t.Fatal("worker did not receive both bindings")
	}
	workerMount := worker.Spec.Template.Spec.Containers[0].VolumeMounts[0]
	if workerMount.MountPath != "/work/task-123" || workerMount.SubPath != "task-123" {
		t.Fatalf("bad worker mount: %+v", workerMount)
	}
	snapshot := worker.Annotations[Annotation]
	if env := worker.Spec.Template.Spec.Containers[0].Env[0]; env.Name != Env || env.Value != snapshot {
		t.Fatal("worker snapshot differs from annotation")
	}
	restarted, err := Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, restarted) {
		t.Fatal("snapshot lost native plan fields")
	}
	executor := testJob()
	if err := restarted.ApplyExecutor(executor, "task-123", []corev1.VolumeMount{{MountPath: "/data/input", ReadOnly: true}, {MountPath: "/results"}}); err != nil {
		t.Fatal(err)
	}
	if len(executor.Spec.Template.Spec.Volumes) != 1 || executor.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "shared-data" {
		t.Fatal("executor did not use workspace claim")
	}
	got := executor.Spec.Template.Spec.Containers[0].VolumeMounts
	if got[0].SubPath != "task-123/data/input" || !got[0].ReadOnly || got[1].SubPath != "task-123/results" {
		t.Fatalf("executor mapping incorrect: %+v", got)
	}
}

func TestWorkspaceAllowsReadOnlyRootView(t *testing.T) {
	c := existingConfig()
	c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("bucket-view", "mountPath: /bucket\nreadOnly: true")}
	p, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	job := testJob()
	if err := p.ApplyWorker(job, "/work", "task-123"); err != nil {
		t.Fatal(err)
	}
	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 2 || mounts[0].ReadOnly || mounts[0].SubPath != "task-123" || !mounts[1].ReadOnly || mounts[1].SubPath != "" || mounts[1].MountPath != "/bucket" {
		t.Fatalf("workspace and root view must preserve their separate access settings: %+v", mounts)
	}
}

func TestS3ReadMountTracksNativeMountPathAndSnapshot(t *testing.T) {
	c := existingConfig()
	c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{
		namedMount("reference-view", "mountPath: /reference\nreadOnly: true\nsubPathExpr: reference-$(POD_NAME)"),
	}
	c.GenericS3 = []*config.GenericS3Storage{mountedS3("reference-bucket", "reference/", "reference-view")}
	p, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	wantReadMount := storage.S3ReadMount{Endpoint: "https://s3.example.test/custom/path", Bucket: "reference-bucket", Prefix: "reference/", Root: "/reference"}
	if len(p.S3ReadMounts) != 1 || p.S3ReadMounts[0] != wantReadMount {
		t.Fatalf("S3 read mount did not preserve configured values and native root: %+v", p.S3ReadMounts)
	}
	if len(p.WorkerMounts) != 1 || p.WorkerMounts[0].MountPath != "/reference" || p.WorkerMounts[0].SubPathExpr != "reference-$(POD_NAME)" || !p.WorkerMounts[0].ReadOnly {
		t.Fatalf("native mount fields changed: %+v", p.WorkerMounts)
	}

	worker := testJob()
	if err := p.ApplyWorker(worker, "/work", "task-123"); err != nil {
		t.Fatal(err)
	}
	if mount := worker.Spec.Template.Spec.Containers[0].VolumeMounts[1]; mount.MountPath != "/reference" || mount.SubPathExpr != "reference-$(POD_NAME)" {
		t.Fatalf("rendered worker mount changed: %+v", mount)
	}

	snapshot, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot, "access-key") || strings.Contains(snapshot, "access-secret") {
		t.Fatalf("storage snapshot contains S3 credentials: %s", snapshot)
	}
	c.Kubernetes.Storage.Bindings[0].WorkerMounts[0].Mount = "mountPath: /changed"
	c.GenericS3[0].MountedRead.Prefix = "changed/"
	c.GenericS3[0].Endpoint = "https://changed.example.test"
	restored, err := Decode(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if restored.S3ReadMounts[0] != wantReadMount || restored.WorkerMounts[0].MountPath != "/reference" || restored.WorkerMounts[0].SubPathExpr != "reference-$(POD_NAME)" {
		t.Fatalf("snapshot changed after config mutation: reads=%+v mounts=%+v", restored.S3ReadMounts, restored.WorkerMounts)
	}

	c.Kubernetes.Storage.Bindings[0].WorkerMounts[0].Mount = "mountPath: /renamed\nreadOnly: true\nsubPathExpr: reference-$(POD_NAME)"
	c.GenericS3[0].MountedRead.Prefix = "reference/"
	c.GenericS3[0].Endpoint = wantReadMount.Endpoint
	renamed, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.S3ReadMounts[0].Root != "/renamed" || renamed.WorkerMounts[0].MountPath != "/renamed" {
		t.Fatalf("renamed mount path did not update S3 read mount and worker mount: reads=%+v mounts=%+v", renamed.S3ReadMounts, renamed.WorkerMounts)
	}
}

func TestS3MountedReadsResolveByNameRegardlessOfMountOrder(t *testing.T) {
	c := existingConfig()
	c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{
		namedMount("protein-view", "mountPath: /views/protein"),
		namedMount("reads-view", "mountPath: /views/reads"),
	}
	c.GenericS3 = []*config.GenericS3Storage{
		mountedS3("reads-bucket", "reads/", "reads-view"),
		mountedS3("protein-bucket", "protein/", "protein-view"),
	}
	first, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if first.S3ReadMounts[0] != (storage.S3ReadMount{Endpoint: "https://s3.example.test/custom/path", Bucket: "reads-bucket", Prefix: "reads/", Root: "/views/reads"}) || first.S3ReadMounts[1] != (storage.S3ReadMount{Endpoint: "https://s3.example.test/custom/path", Bucket: "protein-bucket", Prefix: "protein/", Root: "/views/protein"}) {
		t.Fatalf("named S3 read mounts resolved incorrectly: %+v", first.S3ReadMounts)
	}

	c.Kubernetes.Storage.Bindings[0].WorkerMounts[0], c.Kubernetes.Storage.Bindings[0].WorkerMounts[1] = c.Kubernetes.Storage.Bindings[0].WorkerMounts[1], c.Kubernetes.Storage.Bindings[0].WorkerMounts[0]
	second, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.S3ReadMounts, second.S3ReadMounts) {
		t.Fatalf("mount list order changed name-based S3 reads: first=%+v second=%+v", first.S3ReadMounts, second.S3ReadMounts)
	}
}

func TestInvalidS3MountedReadsFailDuringResolution(t *testing.T) {
	tests := []struct {
		name   string
		change func(*config.Config)
		want   string
	}{
		{
			name: "unknown reference before template parsing",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: "invalid"}}
				c.GenericS3 = []*config.GenericS3Storage{mountedS3("bucket", "input/", "missing-view")}
			},
			want: "unknown worker mount",
		},
		{
			name: "duplicate name",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("same-view", "mountPath: /one"), namedMount("same-view", "mountPath: /two")}
			},
			want: "duplicate worker mount name",
		},
		{
			name: "duplicate across bindings",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("same-view", "mountPath: /one")}
				c.Kubernetes.Storage.Bindings = append(c.Kubernetes.Storage.Bindings, &config.KubernetesStorageBinding{Name: "cache", Source: &config.KubernetesStorageBinding_ExistingClaim{ExistingClaim: "shared-cache"}, WorkerMounts: []*config.KubernetesWorkerMount{namedMount("same-view", "mountPath: /two")}})
			},
			want: "duplicate worker mount name",
		},
		{
			name: "invalid name",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("bad/name", "mountPath: /one")}
			},
			want: "invalid worker mount name",
		},
		{
			name: "invalid reference name",
			change: func(c *config.Config) {
				c.GenericS3 = []*config.GenericS3Storage{mountedS3("bucket", "input/", "bad/name")}
			},
			want: "invalid worker mount reference",
		},
		{
			name: "nil named mount",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{nil}
			},
			want: "nil worker mount",
		},
		{
			name: "disabled S3 backend",
			change: func(c *config.Config) {
				backend := mountedS3("bucket", "input/", "input-view")
				backend.Disabled = true
				c.GenericS3 = []*config.GenericS3Storage{backend}
			},
			want: "disabled or invalid",
		},
		{
			name: "invalid S3 backend",
			change: func(c *config.Config) {
				backend := mountedS3("bucket", "input/", "input-view")
				backend.Secret = ""
				c.GenericS3 = []*config.GenericS3Storage{backend}
			},
			want: "disabled or invalid",
		},
		{
			name: "missing bucket",
			change: func(c *config.Config) {
				c.GenericS3 = []*config.GenericS3Storage{mountedS3("", "input/", "input-view")}
			},
			want: "requires a bucket",
		},
		{
			name: "duplicate API-only backend identity",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("input-view", "mountPath: /input")}
				other := mountedS3("bucket", "", "")
				other.MountedRead = nil
				c.GenericS3 = []*config.GenericS3Storage{mountedS3("bucket", "input/", "input-view"), other}
			},
			want: "duplicate mounted S3 backend",
		},
		{
			name: "duplicate endpoint and bucket",
			change: func(c *config.Config) {
				c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("input-view", "mountPath: /input"), namedMount("other-view", "mountPath: /other")}
				c.GenericS3 = []*config.GenericS3Storage{mountedS3("bucket", "input/", "input-view"), mountedS3("bucket", "other/", "other-view")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := existingConfig()
			tt.change(c)
			_, err := Resolve(&tes.Task{Id: "task-123"}, c)
			if err == nil || (tt.want != "" && !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
	c := existingConfig()
	c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("input-view", "mountPath: /input")}
	c.GenericS3 = []*config.GenericS3Storage{mountedS3("bucket", "input/", "input-view")}
	p, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	p.S3ReadMounts[0].Root = "/unmounted"
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "does not match an explicit worker mount") {
		t.Fatalf("uncovered S3 read mount root accepted: %v", err)
	}
}

func TestS3MountedReadRequiresKubernetesStorage(t *testing.T) {
	for _, change := range []func(*config.Config){
		func(c *config.Config) { c.Kubernetes.Storage = nil },
		func(c *config.Config) { c.Kubernetes = nil },
	} {
		c := existingConfig()
		c.GenericS3 = []*config.GenericS3Storage{mountedS3("bucket", "input/", "input-view")}
		change(c)
		if _, err := Resolve(&tes.Task{Id: "task-123"}, c); err == nil || !strings.Contains(err.Error(), "require Kubernetes.Storage") {
			t.Fatalf("configured S3 mounted read without Kubernetes.Storage was not rejected: %v", err)
		}
	}
}

func TestNamedContainersReceiveStorageMounts(t *testing.T) {
	c := existingConfig()
	c.Kubernetes.Storage.WorkerContainer = "funnel-worker"
	c.Kubernetes.Storage.ExecutorContainer = "task-runner"
	c.Kubernetes.Storage.Bindings = append(c.Kubernetes.Storage.Bindings, &config.KubernetesStorageBinding{
		Name: "cache",
		Source: &config.KubernetesStorageBinding_ExistingClaim{
			ExistingClaim: "shared-cache",
		},
		WorkerMounts: []*config.KubernetesWorkerMount{namedMount("cache-view", "mountPath: /cache\nreadOnly: true\nsubPath: cache")},
	})
	p, err := Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}

	worker := testJob()
	worker.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar"}, {Name: "funnel-worker"}}
	if err := p.ApplyWorker(worker, "/work", "task-123"); err != nil {
		t.Fatal(err)
	}
	containers := worker.Spec.Template.Spec.Containers
	if len(containers[0].VolumeMounts) != 0 || len(containers[0].Env) != 0 {
		t.Fatalf("sidecar received worker storage: %+v", containers[0])
	}
	if len(containers[1].VolumeMounts) != 2 || len(containers[1].Env) != 1 || containers[1].Env[0].Name != Env {
		t.Fatalf("worker container missed storage mounts or snapshot: %+v", containers[1])
	}
	workerSnapshot, err := Decode(worker.Annotations[Annotation])
	if err != nil {
		t.Fatal(err)
	}
	if workerSnapshot.WorkerContainer != "funnel-worker" || workerSnapshot.ExecutorContainer != "task-runner" {
		t.Fatalf("container selection was not persisted: %+v", workerSnapshot)
	}

	executor := testJob()
	executor.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar"}, {Name: "task-runner"}}
	if err := workerSnapshot.ApplyExecutor(executor, "task-123", []corev1.VolumeMount{{MountPath: "/inputs"}}); err != nil {
		t.Fatal(err)
	}
	containers = executor.Spec.Template.Spec.Containers
	if len(containers[0].VolumeMounts) != 0 {
		t.Fatalf("executor sidecar received task storage: %+v", containers[0].VolumeMounts)
	}
	if len(containers[1].VolumeMounts) != 1 || containers[1].VolumeMounts[0].SubPath != "task-123/inputs" {
		t.Fatalf("task container missed workspace mount: %+v", containers[1].VolumeMounts)
	}
}

func TestMultipleContainersRequireStorageContainerSelection(t *testing.T) {
	p, err := Resolve(&tes.Task{Id: "task-123"}, existingConfig())
	if err != nil {
		t.Fatal(err)
	}

	worker := testJob()
	worker.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar"}, {Name: "worker"}}
	if err := p.ApplyWorker(worker, "/work", "task-123"); err == nil || !strings.Contains(err.Error(), "Kubernetes.Storage.WorkerContainer") {
		t.Fatalf("worker template without a selection should fail actionably, got %v", err)
	}

	executor := testJob()
	executor.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar"}, {Name: "runner"}}
	if err := p.ApplyExecutor(executor, "task-123", []corev1.VolumeMount{{MountPath: "/inputs"}}); err == nil || !strings.Contains(err.Error(), "Kubernetes.Storage.ExecutorContainer") {
		t.Fatalf("executor template without a selection should fail actionably, got %v", err)
	}

	c := existingConfig()
	c.Kubernetes.Storage.WorkerContainer = "missing"
	p, err = Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyWorker(testJob(), "/work", "task-123"); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("unknown worker container should be rejected, got %v", err)
	}

	c = existingConfig()
	c.Kubernetes.Storage.ExecutorContainer = "missing"
	p, err = Resolve(&tes.Task{Id: "task-123"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyExecutor(testJob(), "task-123", []corev1.VolumeMount{{MountPath: "/inputs"}}); err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("unknown executor container should be rejected, got %v", err)
	}
}

func TestNativeTemplatesPreservedAndPairBound(t *testing.T) {
	for _, pair := range []bool{false, true} {
		t.Run(map[bool]string{false: "PVC-only", true: "pair"}[pair], func(t *testing.T) {
			c := existingConfig()
			templates := &config.KubernetesStorageTemplates{PVC: pvcTemplate}
			if pair {
				templates.PV = pvTemplate
			}
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: templates}
			c.GenericS3 = nil
			p, err := Resolve(&tes.Task{Id: "task-123"}, c)
			if err != nil {
				t.Fatal(err)
			}
			claim := p.Claims[0]
			if claim.Name != "claim-task-123" || claim.Namespace != "jobs" || *claim.Spec.StorageClassName != "fast" || claim.Spec.Resources.Requests.Storage().String() != "5Gi" || claim.Spec.AccessModes[0] != corev1.ReadWriteMany {
				t.Fatalf("native PVC altered: %+v", claim)
			}
			if pair {
				pv := p.PersistentVolumes[0]
				if pv.Spec.CSI.Driver != "example.csi" || pv.Spec.CSI.VolumeAttributes["bucket"] != "native-bucket" || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
					t.Fatal("native CSI/PV fields altered")
				}
				if claim.Spec.VolumeName != pv.Name || pv.Spec.ClaimRef.Name != claim.Name || pv.Spec.ClaimRef.Namespace != "jobs" {
					t.Fatal("PV/PVC pair was not bound")
				}
			} else if len(p.PersistentVolumes) != 0 || claim.Spec.VolumeName != "" {
				t.Fatal("PVC-only acquired PV")
			}
			snapshot, err := p.Encode()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded.Claims) != 0 || len(decoded.PersistentVolumes) != 0 || !reflect.DeepEqual(p.OwnedClaims, decoded.OwnedClaims) || !reflect.DeepEqual(p.OwnedVolumes, decoded.OwnedVolumes) {
				t.Fatal("snapshot lost ownership or retained creation objects")
			}
		})
	}
}

func TestInvalidBindings(t *testing.T) {
	tests := []struct {
		name   string
		change func(*config.Config)
	}{
		{"unknown workspace", func(c *config.Config) { c.Kubernetes.Storage.WorkspaceVolume = "missing" }},
		{"duplicate binding", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings = append(c.Kubernetes.Storage.Bindings, c.Kubernetes.Storage.Bindings[0])
		}},
		{"empty source", func(c *config.Config) { c.Kubernetes.Storage.Bindings[0].Source = nil }},
		{"invalid claim", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_ExistingClaim{ExistingClaim: "../claim"}
		}},
		{"relative workdir", func(c *config.Config) { c.Worker.WorkDir = "relative" }},
		{"mount traversal", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("data-view", "mountPath: /data/../etc")}
		}},
		{"foreign mount", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("data-view", "name: foreign\nmountPath: /data")}
		}},
		{"single pod workspace", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: strings.Replace(pvcTemplate, "ReadWriteMany", "ReadWriteOncePod", 1)}}
		}},
		{"mixed single pod workspace", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: strings.Replace(pvcTemplate, "ReadWriteMany", "ReadWriteMany, ReadWriteOncePod", 1)}}
		}},
		{"different namespace", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: strings.Replace(pvcTemplate, "  name:", "  namespace: foreign\n  name:", 1)}}
		}},
		{"wrong kind", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: strings.Replace(pvcTemplate, "PersistentVolumeClaim", "Secret", 1)}}
		}},
		{"readonly workspace PVC", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: strings.Replace(pvcTemplate, "ReadWriteMany", "ReadOnlyMany", 1)}}
		}},
		{"owned native PVC", func(c *config.Config) {
			owned := strings.Replace(pvcTemplate, "  name:", "  ownerReferences:\n  - apiVersion: batch/v1\n    kind: Job\n    name: foreign\n    uid: foreign\n  name:", 1)
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: owned}}
		}},
		{"owned native PV", func(c *config.Config) {
			owned := strings.Replace(pvTemplate, "  name:", "  ownerReferences:\n  - apiVersion: batch/v1\n    kind: Job\n    name: foreign\n    uid: foreign\n  name:", 1)
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate, PV: owned}}
		}},
		{"block PVC", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate + "  volumeMode: Block\n"}}
		}},
		{"block PV", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate, PV: pvTemplate + "  volumeMode: Block\n"}}
		}},
		{"workspace overlay", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{namedMount("workspace-view", "mountPath: /work")}
		}},
		{"unknown native field", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate + "  typo: value\n"}}
		}},
		{"multiple docs", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate + "---\n" + pvcTemplate}}
		}},
		{"foreign pair", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate, PV: pvTemplate + "  claimRef:\n    name: foreign\n"}}
		}},
		{"missing PV source", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate, PV: "apiVersion: v1\nkind: PersistentVolume\nmetadata:\n  name: disk-{{.TaskId}}\nspec: {}\n"}}
		}},
		{"multiple PV sources", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: pvcTemplate, PV: pvTemplate + "  hostPath:\n    path: /tmp/host\n"}}
		}},
		{"shared generated name", func(c *config.Config) {
			c.Kubernetes.Storage.Bindings[0].Source = &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: strings.Replace(pvcTemplate, "claim-{{.TaskId}}", "shared", 1)}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := existingConfig()
			tt.change(c)
			if _, err := Resolve(&tes.Task{Id: "task-123"}, c); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestMountFailuresDoNotModifyJob(t *testing.T) {
	p, err := Resolve(&tes.Task{Id: "task-123"}, existingConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range []corev1.VolumeMount{{MountPath: "relative"}, {MountPath: "/a/../b"}, {MountPath: "/a", SubPath: "override"}} {
		job := testJob()
		before := job.DeepCopy()
		if err := p.ApplyExecutor(job, "task-123", []corev1.VolumeMount{mount}); err == nil {
			t.Fatal("bad executor mount accepted")
		}
		if !reflect.DeepEqual(job, before) {
			t.Fatal("failed application modified job")
		}
	}
	job := testJob()
	job.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "workspace"}}
	before := job.DeepCopy()
	if err := p.ApplyWorker(job, "/work", "task-123"); err == nil {
		t.Fatal("volume collision accepted")
	}
	if !reflect.DeepEqual(job, before) {
		t.Fatal("failed application modified job")
	}
	if err := p.ApplyWorker(testJob(), "/different", "task-123"); err == nil {
		t.Fatal("different worker work directory accepted")
	}
	if err := p.ApplyExecutor(testJob(), "other-task", nil); err == nil {
		t.Fatal("different task accepted")
	}
	job = testJob()
	job.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: "/etc/config"}}
	before = job.DeepCopy()
	if err := p.ApplyExecutor(job, "task-123", []corev1.VolumeMount{{MountPath: "/etc"}}); err == nil {
		t.Fatal("parent mount overlay accepted")
	}
	if !reflect.DeepEqual(job, before) {
		t.Fatal("overlay error modified job")
	}
}

func TestInvalidSnapshot(t *testing.T) {
	p, err := Resolve(&tes.Task{Id: "task-123"}, existingConfig())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"null", "{}", snapshot + "{}", strings.Replace(snapshot, `"Version":3`, `"Version":2`, 1), strings.Replace(snapshot, `"Version":3`, `"Version":4`, 1), strings.Replace(snapshot, `"WorkspaceVolume":"workspace"`, `"WorkspaceVolume":"missing"`, 1), strings.Replace(snapshot, `"OwnedClaims":null`, `"OwnedClaims":["unmounted"]`, 1)} {
		if _, err := Decode(invalid); err == nil {
			t.Fatalf("invalid snapshot accepted: %s", invalid)
		}
	}
	if _, err := Decode(strings.Replace(snapshot, `"Version":3`, `"Version":2`, 1)); err == nil || !strings.Contains(err.Error(), "version 3 is required") {
		t.Fatalf("version 2 storage plan snapshot was not rejected clearly: %v", err)
	}
	oldSnapshot := `{"Version":2,"TaskID":"task-123","Namespace":"jobs","WorkDir":"/work","WorkspaceVolume":"workspace","WorkerContainer":"","ExecutorContainer":"","InputRoutes":[{"URLPrefix":"s3://bucket/ref","Root":"/mnt/reference"}],"Volumes":[],"WorkerMounts":[],"OwnedClaims":null,"OwnedVolumes":null}`
	if _, err := Decode(oldSnapshot); err == nil || !strings.Contains(err.Error(), "version 2; version 3 is required") {
		t.Fatalf("actual version 2 fields hid the actionable version error: %v", err)
	}
}

func TestLegacyResolve(t *testing.T) {
	if p, err := Resolve(&tes.Task{Id: "task-123"}, config.DefaultConfig()); err != nil || p != nil {
		t.Fatalf("legacy config produced a plan: %+v %v", p, err)
	}
}

func TestDottedTaskID(t *testing.T) {
	if _, err := Resolve(&tes.Task{Id: "task.123"}, existingConfig()); err != nil {
		t.Fatalf("valid Kubernetes task ID rejected: %v", err)
	}
}

func testJob() *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "task-123", Namespace: "jobs"}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}}}
}
