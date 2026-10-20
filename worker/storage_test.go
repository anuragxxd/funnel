package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-test/deep"
	"github.com/ohsu-comp-bio/funnel/events"
	"github.com/ohsu-comp-bio/funnel/storage"
	"github.com/ohsu-comp-bio/funnel/tes"
)

type fakelist struct {
	list []*storage.Object
	storage.Fake
}

func (f fakelist) List(context.Context, string) ([]*storage.Object, error) {
	return f.list, nil
}

func TestFlattenInputs(t *testing.T) {
	ev := events.NewTaskWriter("task-1", 0, events.Noop{})
	fake := fakelist{
		list: []*storage.Object{
			{URL: "s3://bkt/path/to/one.txt", Name: "path/to/one.txt"},
			{URL: "s3://bkt/path/to/two.txt", Name: "path/to/two.txt"},
		},
	}

	in := []*tes.Input{
		{Url: "s3://bkt/path", Path: "/inputs/foo", Type: tes.Directory},
		{Url: "s3://bkt/path", Path: "/inputs/foo/", Type: tes.Directory},
		{Url: "s3://bkt/path/", Path: "/inputs/foo", Type: tes.Directory},
		{Url: "s3://bkt/path/", Path: "/inputs/foo/", Type: tes.Directory},
	}

	bg := context.Background()
	flat, err := FlattenInputs(bg, in, fake, ev)
	if err != nil {
		t.Fatal(err)
	}

	expected := []*tes.Input{
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
		{Url: "s3://bkt/path/to/one.txt", Path: "/inputs/foo/to/one.txt"},
		{Url: "s3://bkt/path/to/two.txt", Path: "/inputs/foo/to/two.txt"},
	}

	for _, diff := range deep.Equal(flat, expected) {
		t.Error(diff)
	}
}

func TestUploadOutputsFileTypedDirectory(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"nested/one.txt": "first file\n",
		"nested/two.txt": "second file\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	output := &tes.Output{Url: "s3://bucket/outputs", Path: root, Type: tes.File}
	store := &capturingUploadStorage{uploaded: make(map[string]string)}
	logs, err := UploadOutputs(context.Background(), []*tes.Output{output}, store,
		events.NewTaskWriter("task-1", 0, events.Noop{}), 1)
	if err != nil {
		t.Fatal(err)
	}

	wantUploads := map[string]string{
		"s3://bucket/outputs/nested/one.txt": "first file\n",
		"s3://bucket/outputs/nested/two.txt": "second file\n",
	}
	if diff := deep.Equal(store.uploaded, wantUploads); diff != nil {
		t.Fatalf("uploaded files differ: %v", diff)
	}

	wantLogs := []*tes.OutputFileLog{
		{Url: "s3://bucket/outputs/nested/one.txt", Path: filepath.Join(root, "nested/one.txt"), SizeBytes: "11"},
		{Url: "s3://bucket/outputs/nested/two.txt", Path: filepath.Join(root, "nested/two.txt"), SizeBytes: "12"},
	}
	if diff := deep.Equal(logs, wantLogs); diff != nil {
		t.Fatalf("output logs differ: %v", diff)
	}
	if output.Url != "s3://bucket/outputs" || output.Path != root || output.Type != tes.File {
		t.Fatalf("declared output was mutated: %+v", output)
	}
}

func TestUploadOutputsMissingFileStillReachesStorage(t *testing.T) {
	url := "s3://bucket/missing.txt"
	store := &capturingUploadStorage{uploaded: make(map[string]string)}
	logs, err := UploadOutputs(context.Background(), []*tes.Output{
		{Url: url, Path: filepath.Join(t.TempDir(), "missing.txt"), Type: tes.File},
	}, store, events.NewTaskWriter("task-1", 0, events.Noop{}), 1)
	if err == nil {
		t.Fatal("expected missing source file to fail during upload")
	}
	if len(store.attempted) != 1 || store.attempted[0] != url {
		t.Fatalf("storage upload attempts = %v, want [%s]", store.attempted, url)
	}
	if len(logs) != 0 {
		t.Fatalf("output logs = %v, want none", logs)
	}
}

type capturingUploadStorage struct {
	storage.Fake
	uploaded  map[string]string
	attempted []string
}

func (s *capturingUploadStorage) Join(url, suffix string) (string, error) {
	return strings.TrimRight(url, "/") + "/" + filepath.ToSlash(suffix), nil
}

func (s *capturingUploadStorage) Put(_ context.Context, url, sourcePath string) (*storage.Object, error) {
	s.attempted = append(s.attempted, url)
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, err
	}
	s.uploaded[url] = string(data)
	return &storage.Object{URL: url, Size: int64(len(data))}, nil
}
