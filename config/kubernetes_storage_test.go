package config

import (
	"bytes"
	"encoding/gob"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestKubernetesStorageConfigRoundTrip(t *testing.T) {
	input := []byte(`
Kubernetes:
  Storage:
    WorkspaceVolume: shared
    WorkerContainer: funnel-worker
    ExecutorContainer: funnel-executor
    Bindings:
      - Name: shared-inputs
        ExistingClaim: shared-data
        WorkerMounts:
          - |-
            mountPath: /data
            subPath: inputs
      - Name: mounted-s3
        Templates:
          PVC: |-
            apiVersion: v1
            kind: PersistentVolumeClaim
          PV: |-
            apiVersion: v1
            kind: PersistentVolume
`)

	got := EmptyConfig()
	if err := Parse(input, got); err != nil {
		t.Fatal("parse storage config:", err)
	}

	want := &Config{
		Kubernetes: &Kubernetes{
			Storage: &KubernetesStorage{
				WorkspaceVolume:   "shared",
				WorkerContainer:   "funnel-worker",
				ExecutorContainer: "funnel-executor",
				Bindings: []*KubernetesStorageBinding{
					{
						Name:         "shared-inputs",
						Source:       &KubernetesStorageBinding_ExistingClaim{ExistingClaim: "shared-data"},
						WorkerMounts: []string{"mountPath: /data\nsubPath: inputs"},
					},
					{
						Name: "mounted-s3",
						Source: &KubernetesStorageBinding_Templates{Templates: &KubernetesStorageTemplates{
							PVC: "apiVersion: v1\nkind: PersistentVolumeClaim",
							PV:  "apiVersion: v1\nkind: PersistentVolume",
						}},
					},
				},
			},
		},
	}
	if !proto.Equal(got, want) {
		t.Fatalf("parsed storage config mismatch:\n got: %v\nwant: %v", got, want)
	}

	encoded, err := ToYaml(got)
	if err != nil {
		t.Fatal("marshal storage config:", err)
	}
	roundTripped := EmptyConfig()
	if err := Parse(encoded, roundTripped); err != nil {
		t.Fatal("parse marshaled storage config:", err)
	}
	if !proto.Equal(roundTripped, want) {
		t.Fatalf("round-tripped storage config mismatch:\n got: %v\nwant: %v", roundTripped, want)
	}
}

func TestKubernetesStorageAbsentKeepsLegacyConfigShape(t *testing.T) {
	conf := EmptyConfig()
	if err := Parse([]byte("Kubernetes: {}\n"), conf); err != nil {
		t.Fatal("parse legacy Kubernetes config:", err)
	}
	if conf.Kubernetes.Storage != nil {
		t.Fatalf("expected absent Storage to remain unset, got %v", conf.Kubernetes.Storage)
	}
}

func TestKubernetesStorageGobRoundTrip(t *testing.T) {
	want := &Config{
		Kubernetes: &Kubernetes{
			Storage: &KubernetesStorage{
				Bindings: []*KubernetesStorageBinding{
					{
						Name:   "shared-inputs",
						Source: &KubernetesStorageBinding_ExistingClaim{ExistingClaim: "shared-data"},
					},
					{
						Name: "mounted-s3",
						Source: &KubernetesStorageBinding_Templates{Templates: &KubernetesStorageTemplates{
							PVC: "kind: PersistentVolumeClaim",
							PV:  "kind: PersistentVolume",
						}},
					},
				},
			},
		},
	}

	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(want); err != nil {
		t.Fatal("encode config over gob:", err)
	}

	var got Config
	if err := gob.NewDecoder(&encoded).Decode(&got); err != nil {
		t.Fatal("decode config over gob:", err)
	}
	if !proto.Equal(&got, want) {
		t.Fatalf("gob round-trip mismatch:\n got: %v\nwant: %v", &got, want)
	}
}
