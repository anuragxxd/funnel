package config

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
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
          - Name: input-view
            Mount: |-
              mountPath: /data
              subPath: inputs
          - Name: output-view
            Mount: |-
              mountPath: /outputs
      - Name: mounted-s3
        Templates:
          PVC: |-
            apiVersion: v1
            kind: PersistentVolumeClaim
          PV: |-
            apiVersion: v1
            kind: PersistentVolume
GenericS3:
  - Endpoint: https://s3.example.test
    Key: access
    Secret: secret
    Bucket: bucket
    MountedRead:
      WorkerMount: input-view
      Prefix: prefix/
    MountedWrite:
      WorkerMount: output-view
      Prefix: results/
      Timeout: 90s
`)

	got := EmptyConfig()
	if err := Parse(input, got); err != nil {
		t.Fatal("parse storage config:", err)
	}

	want := &Config{
		GenericS3: []*GenericS3Storage{{
			Endpoint: "https://s3.example.test",
			Key:      "access",
			Secret:   "secret",
			Bucket:   "bucket",
			MountedWrite: &S3MountedWrite{
				WorkerMount: "output-view",
				Prefix:      "results/",
				Timeout:     durationpb.New(90 * time.Second),
			},
			MountedRead: &S3MountedRead{
				WorkerMount: "input-view",
				Prefix:      "prefix/",
			},
		}},
		Kubernetes: &Kubernetes{
			Storage: &KubernetesStorage{
				WorkspaceVolume:   "shared",
				WorkerContainer:   "funnel-worker",
				ExecutorContainer: "funnel-executor",
				Bindings: []*KubernetesStorageBinding{
					{
						Name:   "shared-inputs",
						Source: &KubernetesStorageBinding_ExistingClaim{ExistingClaim: "shared-data"},
						WorkerMounts: []*KubernetesWorkerMount{
							{Name: "input-view", Mount: "mountPath: /data\nsubPath: inputs"},
							{Name: "output-view", Mount: "mountPath: /outputs"},
						},
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

	configDescriptor := (&Config{}).ProtoReflect().Descriptor()
	if configDescriptor.Fields().ByName("MountedInputs") != nil {
		t.Fatal("Config.MountedInputs must be removed")
	}
	if !configDescriptor.ReservedRanges().Has(34) || !configDescriptor.ReservedNames().Has("MountedInputs") {
		t.Fatal("Config.MountedInputs field 34 must be reserved")
	}
	storageDescriptor := (&KubernetesStorage{}).ProtoReflect().Descriptor()
	if storageDescriptor.Fields().ByName("MountedInputs") != nil {
		t.Fatal("KubernetesStorage.MountedInputs must be removed")
	}
	if !storageDescriptor.ReservedRanges().Has(5) || !storageDescriptor.ReservedNames().Has("MountedInputs") {
		t.Fatal("KubernetesStorage.MountedInputs field 5 must be reserved")
	}
	s3MountedRead := (&GenericS3Storage{}).ProtoReflect().Descriptor().Fields().ByName("MountedRead")
	if s3MountedRead == nil || s3MountedRead.Number() != 9 || s3MountedRead.Kind() != protoreflect.MessageKind {
		t.Fatalf("GenericS3Storage.MountedRead field = %v, want optional message field 9", s3MountedRead)
	}
	s3MountedWrite := (&GenericS3Storage{}).ProtoReflect().Descriptor().Fields().ByName("MountedWrite")
	if s3MountedWrite == nil || s3MountedWrite.Number() != 10 || s3MountedWrite.Kind() != protoreflect.MessageKind {
		t.Fatalf("GenericS3Storage.MountedWrite field = %v, want optional message field 10", s3MountedWrite)
	}
	readFields := (&S3MountedRead{}).ProtoReflect().Descriptor().Fields()
	if field := readFields.ByName("WorkerMount"); field == nil || field.Number() != 1 {
		t.Fatalf("S3MountedRead.WorkerMount field = %v, want 1", field)
	}
	if field := readFields.ByName("Prefix"); field == nil || field.Number() != 2 {
		t.Fatalf("S3MountedRead.Prefix field = %v, want 2", field)
	}
	writeFields := (&S3MountedWrite{}).ProtoReflect().Descriptor().Fields()
	for name, number := range map[protoreflect.Name]protoreflect.FieldNumber{
		"WorkerMount": 1,
		"Prefix":      2,
		"Timeout":     3,
	} {
		if field := writeFields.ByName(name); field == nil || field.Number() != number {
			t.Fatalf("S3MountedWrite.%s field = %v, want %d", name, field, number)
		}
	}
	if configDescriptor.ParentFile().Messages().ByName("KubernetesMountedInput") != nil {
		t.Fatal("KubernetesMountedInput message must be removed")
	}
	bindingDescriptor := (&KubernetesStorageBinding{}).ProtoReflect().Descriptor()
	if field := bindingDescriptor.Fields().ByName("WorkerMounts"); field == nil || field.Number() != 5 {
		t.Fatalf("KubernetesStorageBinding.WorkerMounts field = %v, want 5", field)
	}
	if !bindingDescriptor.ReservedRanges().Has(4) {
		t.Fatal("KubernetesStorageBinding field 4 must be reserved")
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

func TestOldMountedInputSchemaIsRejected(t *testing.T) {
	for name, raw := range map[string]string{
		"top-level mounted inputs": `MountedInputs:
  - URLPrefix: s3://bucket/prefix/
    Path: /mounted/input
`,
		"string worker mount": `Kubernetes:
  Storage:
    Bindings:
      - Name: data
        ExistingClaim: shared-data
        WorkerMounts:
          - mountPath: /data
`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := Parse([]byte(raw), EmptyConfig()); err == nil {
				t.Fatal("old mounted input schema was accepted")
			}
		})
	}
}
