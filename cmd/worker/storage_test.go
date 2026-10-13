package worker

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohsu-comp-bio/funnel/compute/kubernetes/storageplan"
	"github.com/ohsu-comp-bio/funnel/config"
	"github.com/ohsu-comp-bio/funnel/logger"
	"github.com/ohsu-comp-bio/funnel/tes"
)

func TestWorkerUsesSubmittedStorageSnapshot(t *testing.T) {
	submitted := config.DefaultConfig()
	submitted.Kubernetes.JobsNamespace = "submitted-tasks"
	submitted.Worker.WorkDir = "/submitted-work"
	mount := t.TempDir()
	if err := os.WriteFile(filepath.Join(mount, "input"), []byte("submitted mount"), 0600); err != nil {
		t.Fatal(err)
	}
	submitted.MountedInputs = []*config.MountedInput{{URLPrefix: "s3://bucket/ref", Path: mount}}
	submitted.Kubernetes.Storage = &config.KubernetesStorage{
		WorkspaceVolume: "workspace",
		Bindings:        []*config.KubernetesStorageBinding{{Name: "workspace", Source: &config.KubernetesStorageBinding_ExistingClaim{ExistingClaim: "submitted-claim"}}},
	}
	plan, err := storageplan.Resolve(&tes.Task{Id: "snapshot-test"}, submitted)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := plan.Encode()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(storageplan.Env, snapshot)
	conf := config.DefaultConfig()
	conf.Compute = "kubernetes"
	conf.EventWriters = nil
	conf.GoogleStorage.Disabled = true
	conf.AmazonS3.Disabled = true
	conf.Worker.WorkDir = "/different-work"
	conf.Kubernetes.JobsNamespace = "different-tasks"
	conf.MountedInputs = []*config.MountedInput{{URLPrefix: "s3://bucket/ref", Path: "/different-mount"}}
	opts := &Options{TaskBase64: base64.StdEncoding.EncodeToString([]byte(`{"id":"snapshot-test","executors":[{"image":"alpine:3.21","command":["true"]}]}`))}
	w, err := NewWorker(context.Background(), conf, logger.NewLogger("test", logger.DefaultConfig()), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Conf.WorkDir != "/submitted-work" || w.Executor.JobsNamespace != "submitted-tasks" || w.Executor.StoragePlan.Volumes[0].PersistentVolumeClaim.ClaimName != "submitted-claim" {
		t.Fatalf("worker did not use submitted storage plan: %+v", w.Executor)
	}
	if conf.Worker.WorkDir != "/different-work" || conf.Kubernetes.JobsNamespace != "different-tasks" {
		t.Fatal("worker startup changed the supplied configuration")
	}
	destination := filepath.Join(t.TempDir(), "input")
	if _, err := w.Store.Get(context.Background(), "s3://bucket/ref/input", destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "submitted mount" || conf.MountedInputs[0].Path != "/different-mount" {
		t.Fatal("worker did not use the submitted mount independently of its static config")
	}
}
