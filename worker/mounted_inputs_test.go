package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohsu-comp-bio/funnel/config"
	"github.com/ohsu-comp-bio/funnel/events"
	"github.com/ohsu-comp-bio/funnel/storage"
	"github.com/ohsu-comp-bio/funnel/tes"
)

func TestDownloadMountedInputDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a b#%.txt"), []byte("input-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a%20b.txt"), []byte("literal percent key"), 0600); err != nil {
		t.Fatal(err)
	}
	conf := config.EmptyConfig()
	conf.GenericS3 = []*config.GenericS3Storage{{Endpoint: "http://s3.invalid", Bucket: "bucket", Key: "key", Secret: "secret"}}
	store, err := storage.NewWorkerMux(conf, []storage.S3ReadMount{{Endpoint: "http://s3.invalid", Bucket: "bucket", Prefix: "ref", Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "inputs")
	ev := events.NewTaskWriter("mounted", 0, events.Noop{})
	if err := DownloadInputs(context.Background(), []*tes.Input{{Url: "s3://bucket/ref", Path: dst, Type: tes.Directory}}, store, ev, 2); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "nested", "a b#%.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "input-data" {
		t.Fatalf("wrong copied input: %q", data)
	}
	literal, err := os.ReadFile(filepath.Join(dst, "a%20b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(literal) != "literal percent key" {
		t.Fatalf("literal S3 key changed during transfer: %q", literal)
	}
}

func TestFlattenInputsRejectsOutsideDirectory(t *testing.T) {
	for _, child := range []string{"s3://bucket/elsewhere/file", "s3://other/ref/file", "s3://bucket/ref/../file"} {
		t.Run(child, func(t *testing.T) {
			_, err := FlattenInputs(context.Background(), []*tes.Input{{Url: "s3://bucket/ref", Path: t.TempDir(), Type: tes.Directory}}, fakelist{list: []*storage.Object{{URL: child}}}, events.NewTaskWriter("mounted", 0, events.Noop{}))
			if err == nil {
				t.Fatal("directory listing escaped input destination")
			}
		})
	}
}
