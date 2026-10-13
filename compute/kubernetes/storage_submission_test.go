package kubernetes

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ohsu-comp-bio/funnel/compute/kubernetes/storageplan"
	"github.com/ohsu-comp-bio/funnel/config"
	"github.com/ohsu-comp-bio/funnel/events"
	"github.com/ohsu-comp-bio/funnel/logger"
	plugin "github.com/ohsu-comp-bio/funnel/plugins/proto"
	"github.com/ohsu-comp-bio/funnel/tes"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const boundClaimTemplate = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: work-{{.TaskId}}
spec:
  storageClassName: research
  accessModes: [ReadWriteMany]
  resources:
    requests:
      storage: 1Gi
`

const boundVolumeTemplate = `apiVersion: v1
kind: PersistentVolume
metadata:
  name: data-{{.TaskId}}
spec:
  storageClassName: research
  capacity:
    storage: 1Gi
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: example.csi.invalid
    volumeHandle: data-{{.TaskId}}
`

func bindingConfig(t *testing.T, binding *config.KubernetesStorageBinding) *config.Config {
	t.Helper()
	conf := config.DefaultConfig()
	workerTemplate, err := os.ReadFile("../../config/kubernetes/worker-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	conf.Kubernetes.WorkerTemplate = string(workerTemplate)
	conf.Kubernetes.Namespace = "server"
	conf.Kubernetes.JobsNamespace = "tasks"
	conf.Kubernetes.ServiceAccountTemplate = ""
	conf.Kubernetes.RoleTemplate = ""
	conf.Kubernetes.RoleBindingTemplate = ""
	conf.Kubernetes.Storage = &config.KubernetesStorage{WorkspaceVolume: binding.Name, Bindings: []*config.KubernetesStorageBinding{binding}}
	conf.GenericS3 = nil
	return conf
}

func existingBinding(claim string) *config.KubernetesStorageBinding {
	return &config.KubernetesStorageBinding{Name: "workspace", Source: &config.KubernetesStorageBinding_ExistingClaim{ExistingClaim: claim}}
}

func createdBinding(pv string) *config.KubernetesStorageBinding {
	return &config.KubernetesStorageBinding{Name: "workspace", Source: &config.KubernetesStorageBinding_Templates{Templates: &config.KubernetesStorageTemplates{PVC: boundClaimTemplate, PV: pv}}}
}

func bindingClient() *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		job.UID = types.UID("uid-" + job.Name)
		return false, nil, nil
	})
	return client
}

func TestStorageBindingsSubmitAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding *config.KubernetesStorageBinding
		claim   string
		ownsPV  bool
	}{
		{"existing", existingBinding("research-data"), "research-data", false},
		{"claim", createdBinding(""), "work-task-a", false},
		{"pair", createdBinding(boundVolumeTemplate), "work-task-a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			client := bindingClient()
			conf := bindingConfig(t, tc.binding)
			external := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "research-data", Namespace: "tasks", Labels: map[string]string{"owner": "operator"}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}}}
			if _, err := client.CoreV1().PersistentVolumeClaims("tasks").Create(ctx, external, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
			task := &tes.Task{Id: "task-a", Inputs: []*tes.Input{{Path: "/inputs/a", Url: "https://example.org/a"}}}
			if err := backend.Submit(ctx, task, conf); err != nil {
				t.Fatal(err)
			}
			job, err := client.BatchV1().Jobs("tasks").Get(ctx, task.Id, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := storageplan.Decode(job.Annotations[storageplan.Annotation])
			if err != nil {
				t.Fatal(err)
			}
			if plan.Volumes[0].PersistentVolumeClaim.ClaimName != tc.claim || plan.Namespace != "tasks" {
				t.Fatalf("wrong submission snapshot: %+v", plan)
			}
			if tc.name != "existing" {
				claim, err := client.CoreV1().PersistentVolumeClaims("tasks").Get(ctx, tc.claim, metav1.GetOptions{})
				if err != nil || len(claim.OwnerReferences) != 1 || claim.OwnerReferences[0].UID != job.UID {
					t.Fatalf("claim must belong to submitted Job: claim=%v error=%v", claim, err)
				}
			}
			if err := backend.Cancel(ctx, task.Id); err != nil {
				t.Fatal(err)
			}
			// Retrying cleanup must remain safe after the Job snapshot is gone.
			if err := backend.Cancel(ctx, task.Id); err != nil {
				t.Fatal(err)
			}
			if tc.ownsPV {
				if _, err := client.CoreV1().PersistentVolumes().Get(ctx, "data-task-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatalf("owned PV remained after cancellation: %v", err)
				}
			} else {
				for _, action := range client.Actions() {
					if action.GetResource().Resource == "persistentvolumes" {
						t.Fatalf("namespaced binding accessed PV API: %v", action)
					}
				}
			}
			claim, err := client.CoreV1().PersistentVolumeClaims("tasks").Get(ctx, external.Name, metav1.GetOptions{})
			if err != nil || claim.Labels["owner"] != "operator" || len(claim.OwnerReferences) != 0 {
				t.Fatalf("external claim changed: %v %v", claim, err)
			}
		})
	}
}

func TestStoragePlanSurvivesConfigurationChange(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	conf := bindingConfig(t, createdBinding(boundVolumeTemplate))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(ctx, &tes.Task{Id: "snapshot"}, conf); err != nil {
		t.Fatal(err)
	}
	// A replacement backend has different configuration but reads the old Job's ownership.
	restarted := &Backend{client: client, conf: bindingConfig(t, existingBinding("other-claim")), event: &noopEventWriter{}, log: backend.log}
	if err := restarted.Cancel(ctx, "snapshot"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Get(ctx, "data-snapshot", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cleanup did not use persisted ownership: %v", err)
	}
}

func TestStorageSubmissionFailureCleansOwnedPV(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})
	conf := bindingConfig(t, createdBinding(boundVolumeTemplate))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(ctx, &tes.Task{Id: "failure"}, conf); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("expected claim creation error, got %v", err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Get(ctx, "data-failure", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("failed submission left owned PV: %v", err)
	}
}

func TestStorageSubmissionPreservesCreationAndRollbackErrors(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	creationErr := errors.New("quota exceeded")
	rollbackErr := errors.New("PV deletion unavailable")
	client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, creationErr
	})
	client.PrependReactor("delete", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, rollbackErr
	})
	conf := bindingConfig(t, createdBinding(boundVolumeTemplate))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	err := backend.Submit(ctx, &tes.Task{Id: "rollback-failure"}, conf)
	if !errors.Is(err, creationErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("submission must retain both creation and cleanup failures: %v", err)
	}
	job, err := client.BatchV1().Jobs("tasks").Get(ctx, "rollback-failure", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Finalizers) == 0 || job.Annotations[storageplan.Annotation] == "" {
		t.Fatal("failed rollback lost its cleanup record")
	}
}

func TestStoragePluginOverridesAreTaskLocal(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	conf := bindingConfig(t, existingBinding("default-claim"))
	conf.Plugins = &config.Plugins{}
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	for _, id := range []string{"alice", "bob"} {
		if _, err := client.CoreV1().PersistentVolumeClaims("tasks").Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: id + "-claim", Namespace: "tasks"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		override := &config.Config{Kubernetes: &config.Kubernetes{Storage: bindingConfig(t, existingBinding(id+"-claim")).Kubernetes.Storage}}
		taskCtx := context.WithValue(ctx, "pluginResponse", &plugin.JobResponse{Config: override})
		if err := backend.WriteEvent(taskCtx, events.NewTaskCreated(&tes.Task{Id: id})); err != nil {
			t.Fatal(err)
		}
		job, err := client.BatchV1().Jobs("tasks").Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := storageplan.Decode(job.Annotations[storageplan.Annotation])
		if err != nil || plan.Volumes[0].PersistentVolumeClaim.ClaimName != id+"-claim" {
			t.Fatalf("wrong tenant binding: %v %v", plan, err)
		}
	}
	if conf.Kubernetes.Storage.Bindings[0].GetExistingClaim() != "default-claim" {
		t.Fatal("plugin mutated server defaults")
	}
}

func TestInvalidStorageFailsBeforeCreatingResources(t *testing.T) {
	client := bindingClient()
	conf := bindingConfig(t, existingBinding("shared"))
	conf.Kubernetes.Storage.WorkspaceVolume = "missing"
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(context.Background(), &tes.Task{Id: "invalid"}, conf); err == nil {
		t.Fatal("invalid binding succeeded")
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("invalid binding reached Kubernetes: %v", client.Actions())
	}
}

func TestMountedInputReferencesResolveBeforeResourceCreation(t *testing.T) {
	for _, mountName := range []string{"missing", "reference-root"} {
		t.Run(mountName, func(t *testing.T) {
			ctx := context.Background()
			client := bindingClient()
			conf := bindingConfig(t, existingBinding("shared"))
			conf.Kubernetes.Storage.Bindings[0].WorkerMounts = []*config.KubernetesWorkerMount{{Name: "reference-root", Mount: "mountPath: /reference\nreadOnly: true"}}
			conf.Kubernetes.Storage.MountedInputs = []*config.KubernetesMountedInput{{URLPrefix: "s3://reference", WorkerMount: mountName}}
			if _, err := client.CoreV1().PersistentVolumeClaims("tasks").Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "tasks"}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
			err := backend.Submit(ctx, &tes.Task{Id: "input-binding"}, conf)
			if mountName == "missing" {
				if err == nil || !strings.Contains(err.Error(), "unknown worker mount") {
					t.Fatalf("missing reference was not rejected: %v", err)
				}
				if len(client.Actions()) != 0 {
					t.Fatalf("invalid input reference reached Kubernetes: %v", client.Actions())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			job, err := client.BatchV1().Jobs("tasks").Get(ctx, "input-binding", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := storageplan.Decode(job.Annotations[storageplan.Annotation])
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.InputRoutes) != 1 || plan.InputRoutes[0].Root != "/reference" || plan.InputRoutes[0].URLPrefix != "s3://reference" {
				t.Fatalf("submitted Job lost the named input binding: %+v", plan.InputRoutes)
			}
		})
	}
}

func TestStorageCleanupRetriesAfterPVDeleteFailure(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	conf := bindingConfig(t, createdBinding(boundVolumeTemplate))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(ctx, &tes.Task{Id: "retry"}, conf); err != nil {
		t.Fatal(err)
	}
	fail := true
	client.PrependReactor("delete", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			fail = false
			return true, nil, errors.New("temporarily unavailable")
		}
		return false, nil, nil
	})
	if err := backend.Cancel(ctx, "retry"); err == nil {
		t.Fatal("expected transient deletion failure")
	}
	job, err := client.BatchV1().Jobs("tasks").Get(ctx, "retry", metav1.GetOptions{})
	if err != nil || job.Annotations[storageplan.Annotation] == "" || len(job.Finalizers) == 0 {
		t.Fatalf("failed cleanup lost its ownership snapshot: %v %v", job, err)
	}
	backend.conf = bindingConfig(t, existingBinding("different-default"))
	if err := backend.Cancel(ctx, "retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().PersistentVolumes().Get(ctx, "data-retry", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retry left owned PV: %v", err)
	}
}

func TestStorageCleanupRetainsFinalizerAfterJobDeleteFailure(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	conf := bindingConfig(t, createdBinding(boundVolumeTemplate))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(ctx, &tes.Task{Id: "retry-job"}, conf); err != nil {
		t.Fatal(err)
	}
	fail := true
	client.PrependReactor("delete", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			fail = false
			return true, nil, errors.New("temporarily unavailable")
		}
		return false, nil, nil
	})
	if err := backend.Cancel(ctx, "retry-job"); err == nil {
		t.Fatal("expected transient Job deletion failure")
	}
	job, err := client.BatchV1().Jobs("tasks").Get(ctx, "retry-job", metav1.GetOptions{})
	if err != nil || len(job.Finalizers) == 0 {
		t.Fatalf("failed Job deletion lost its cleanup finalizer: %v %v", job, err)
	}
	backend.conf = bindingConfig(t, existingBinding("different-default"))
	backend.database = &mockDatabase{tasks: map[string]*tes.Task{"retry-job": {Id: "retry-job", State: tes.Complete}}}
	backend.CleanOrphanedResources(ctx)
	if _, err := client.BatchV1().Jobs("tasks").Get(ctx, "retry-job", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan cleanup did not retry Job deletion: %v", err)
	}
}

func TestStorageOrphanCleanupUsesFinalizedJobAfterConfigChange(t *testing.T) {
	ctx := context.Background()
	client := bindingClient()
	conf := bindingConfig(t, createdBinding(boundVolumeTemplate))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(ctx, &tes.Task{Id: "orphan"}, conf); err != nil {
		t.Fatal(err)
	}
	backend.conf = bindingConfig(t, existingBinding("different-default"))
	backend.database = &mockDatabase{tasks: map[string]*tes.Task{"orphan": {Id: "orphan", State: tes.Complete}}}
	client.ClearActions()
	backend.CleanOrphanedResources(ctx)
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "persistentvolumes" && action.GetVerb() == "list" {
			t.Fatal("namespaced configuration must discover ownership through Jobs")
		}
	}
	if _, err := client.CoreV1().PersistentVolumes().Get(ctx, "data-orphan", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan cleanup lost submitted PV names: %v", err)
	}
}

func TestStorageNamespaceOverrideRejectedBeforeCreation(t *testing.T) {
	client := bindingClient()
	conf := bindingConfig(t, existingBinding("shared"))
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	override := bindingConfig(t, existingBinding("shared"))
	override.Kubernetes.JobsNamespace = "another-namespace"
	if err := backend.Submit(context.Background(), &tes.Task{Id: "namespace"}, override); err == nil || !strings.Contains(err.Error(), "JobsNamespace") {
		t.Fatalf("expected namespace override error, got %v", err)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("namespace override reached Kubernetes: %v", client.Actions())
	}
}

func TestStorageSinglePodWorkspaceRejected(t *testing.T) {
	client := bindingClient()
	binding := createdBinding("")
	binding.GetTemplates().PVC = strings.ReplaceAll(boundClaimTemplate, "ReadWriteMany", "ReadWriteOncePod")
	conf := bindingConfig(t, binding)
	backend := &Backend{client: client, conf: conf, event: &noopEventWriter{}, log: logger.NewLogger("test", logger.DefaultConfig())}
	if err := backend.Submit(context.Background(), &tes.Task{Id: "single-pod"}, conf); err == nil {
		t.Fatal("single-pod workspace cannot serve worker and executor")
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("unsupported workspace reached Kubernetes: %v", client.Actions())
	}
}
