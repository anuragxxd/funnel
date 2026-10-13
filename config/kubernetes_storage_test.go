package config

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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
	input = append(input, []byte(`
MountedInputs:
  - URLPrefix: s3://bucket/prefix/
    Path: /mounted/input
`)...)

	got := EmptyConfig()
	if err := Parse(input, got); err != nil {
		t.Fatal("parse storage config:", err)
	}

	want := &Config{
		MountedInputs: []*MountedInput{{URLPrefix: "s3://bucket/prefix/", Path: "/mounted/input"}},
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

func TestKubernetesSchemaFieldCompatibility(t *testing.T) {
	kubernetes := (&Kubernetes{}).ProtoReflect().Descriptor()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{
		"ForbiddenPathPrefixes": 20,
		"WorkerImage":           24,
		"Storage":               25,
	} {
		field := kubernetes.Fields().ByName(name)
		if field == nil {
			t.Fatalf("Kubernetes field %q is missing", name)
		}
		if field.Number() != number {
			t.Errorf("Kubernetes field %q number = %d, want %d", name, field.Number(), number)
		}
	}
	for _, number := range []protoreflect.FieldNumber{21, 22, 23} {
		if !kubernetes.ReservedRanges().Has(number) {
			t.Errorf("Kubernetes field number %d is not reserved", number)
		}
	}
	for _, name := range []protoreflect.Name{"PVCMode", "StorageClassName", "SharedPVCName", "S3FilesFilesystemId"} {
		if !kubernetes.ReservedNames().Has(name) {
			t.Errorf("Kubernetes field name %q is not reserved", name)
		}
	}

	mountedInputs := (&Config{}).ProtoReflect().Descriptor().Fields().ByName("MountedInputs")
	if mountedInputs == nil || mountedInputs.Number() != 34 {
		t.Fatalf("Config.MountedInputs field number = %v, want 34", mountedInputs)
	}
}

func TestRemovedKubernetesStorageKeysAreRejected(t *testing.T) {
	for _, name := range []string{"PVCMode", "StorageClassName", "SharedPVCName", "S3FilesFilesystemId"} {
		t.Run(name, func(t *testing.T) {
			conf := EmptyConfig()
			raw := []byte(fmt.Sprintf("Kubernetes:\n  %s: true\n", name))
			err := Parse(raw, conf)
			if err == nil {
				t.Fatalf("expected removed field %q to be rejected", name)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf(`unknown field %q`, name)) {
				t.Fatalf("expected unknown field error for %q, got %v", name, err)
			}
		})
	}
}
