package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohsu-comp-bio/funnel/config"
)

type mountedFallback struct {
	Fake
	calls []string
}

func (f *mountedFallback) Get(_ context.Context, u, p string) (*Object, error) {
	f.calls = append(f.calls, "get:"+u)
	return &Object{URL: u, Size: 6}, os.WriteFile(p, []byte("remote"), 0600)
}
func (f *mountedFallback) Put(_ context.Context, u, _ string) (*Object, error) {
	f.calls = append(f.calls, "put:"+u)
	return &Object{URL: u, Size: 9, ETag: "api-receipt"}, nil
}
func (f *mountedFallback) Stat(_ context.Context, u string) (*Object, error) {
	f.calls = append(f.calls, "stat:"+u)
	return &Object{URL: u}, nil
}
func (f *mountedFallback) List(_ context.Context, u string) ([]*Object, error) {
	f.calls = append(f.calls, "list:"+u)
	return []*Object{{URL: u + "/remote"}}, nil
}

func mountedFixture(t *testing.T) (*MountedInputs, *mountedFallback, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a b.txt"), []byte("mounted"), 0600); err != nil {
		t.Fatal(err)
	}
	fallback := &mountedFallback{}
	s, err := WithMountedInputs(fallback, []*config.MountedInput{{URLPrefix: "s3://bucket/reference/", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	return s, fallback, root
}

func TestMountedInputsCopyAndList(t *testing.T) {
	s, fallback, root := mountedFixture(t)
	ctx := context.Background()
	objects, err := s.List(ctx, "s3://bucket/reference/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].URL != "s3://bucket/reference/nested/a b.txt" {
		t.Fatalf("incorrect listing: %+v", objects)
	}
	dst := filepath.Join(t.TempDir(), "input")
	obj, err := s.Get(ctx, objects[0].URL, dst)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mounted" || obj.Size != 7 || len(fallback.calls) != 0 {
		t.Fatalf("read did not use mount: %q %+v %v", data, obj, fallback.calls)
	}
	if err := os.WriteFile(dst, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, "nested", "a b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != "mounted" {
		t.Fatal("copy aliased source instead of making a workspace copy")
	}
	if _, err := s.Get(ctx, "s3://bucket/reference/missing", dst); err == nil {
		t.Fatal("missing mounted input silently fell back")
	}
	if len(fallback.calls) != 0 {
		t.Fatal("matched missing input reached remote backend")
	}
}

func TestMountedInputsFallbackAndAPIPublication(t *testing.T) {
	s, fallback, root := mountedFixture(t)
	ctx := context.Background()
	for _, u := range []string{"s3://bucket/reference-other/file", "s3://other/reference/file", "https://example.org/file", "s3://other/a/../key", "s3://other/a//b", "s3://other/a#b?c%", "s3://bucket/reference-other/%2e%2e/file"} {
		if _, err := s.Get(ctx, u, filepath.Join(t.TempDir(), "input")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Stat(ctx, "s3://other/file"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(ctx, "s3://other/dir"); err != nil {
		t.Fatal(err)
	}
	obj, err := s.Put(ctx, "s3://bucket/reference/result", filepath.Join(root, "nested", "a b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if obj.ETag != "api-receipt" || len(fallback.calls) != 10 || fallback.calls[9] != "put:s3://bucket/reference/result" {
		t.Fatalf("output bypassed API: %+v %v", obj, fallback.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "result")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output wrote into input mount: %v", err)
	}
}

func TestMountedInputsRejectEscapes(t *testing.T) {
	s, fallback, root := mountedFixture(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"s3://bucket/reference/../secret", "s3://bucket/reference/%2e%2e/secret", "s3://bucket/reference/x%2f..%2fsecret", "s3://bucket/reference/escape/secret", "s3://bucket/reference/%5csecret", "s3://bucket/reference/%00"} {
		t.Run(u, func(t *testing.T) {
			if _, err := s.Get(context.Background(), u, filepath.Join(t.TempDir(), "out")); err == nil {
				t.Fatal("unsafe input accepted")
			}
			if _, err := s.Stat(context.Background(), u); err == nil {
				t.Fatal("unsafe stat accepted")
			}
		})
	}
	if _, err := s.List(context.Background(), "s3://bucket/reference"); err == nil {
		t.Fatal("listing followed escaping symlink")
	}
	if len(fallback.calls) != 0 {
		t.Fatal("unsafe input fell back to API")
	}
}

func TestMountedReadValidationDoesNotRestrictAPIOutputs(t *testing.T) {
	s, fallback, root := mountedFixture(t)
	u := "s3://bucket/reference/%2e%2e/result"
	unsupported := s.UnsupportedOperations(u)
	if unsupported.Get == nil || unsupported.Stat == nil || unsupported.List == nil {
		t.Fatal("unsafe mounted reads were permitted")
	}
	if unsupported.Put != nil || unsupported.Join != nil {
		t.Fatalf("mounted read policy blocked API outputs: %+v", unsupported)
	}
	obj, err := s.Put(context.Background(), u, filepath.Join(root, "nested", "a b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if obj.ETag != "api-receipt" || len(fallback.calls) != 1 || fallback.calls[0] != "put:"+u {
		t.Fatalf("output did not use the API backend: %+v %v", obj, fallback.calls)
	}
}

func TestMountedInputsCancellationAndValidation(t *testing.T) {
	s, _, root := mountedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Get(ctx, "s3://bucket/reference/nested/a b.txt", filepath.Join(t.TempDir(), "out")); !errors.Is(err, context.Canceled) {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.List(ctx, "s3://bucket/reference"); !errors.Is(err, context.Canceled) {
		t.Fatalf("list: %v", err)
	}
	for _, mappings := range [][]*config.MountedInput{
		{nil},
		{{URLPrefix: "file:///tmp", Path: root}},
		{{URLPrefix: "s3://bucket", Path: "relative"}},
		{{URLPrefix: "s3://bucket/../bad", Path: root}},
		{{URLPrefix: "s3://bucket?query", Path: root}},
		{{URLPrefix: "s3://bucket/ref", Path: root}, {URLPrefix: "s3://bucket/ref/sub", Path: root}},
		{{URLPrefix: "s3://bucket/ref", Path: root}, {URLPrefix: "s3://bucket/ref/", Path: root}},
	} {
		if _, err := WithMountedInputs(&Fake{}, mappings); err == nil {
			t.Fatalf("invalid mapping accepted: %+v", mappings)
		}
	}
	if _, err := WithMountedInputs(&Fake{}, []*config.MountedInput{{URLPrefix: "s3://bucket/ref", Path: root}, {URLPrefix: "s3://bucket/refs", Path: root}}); err != nil {
		t.Fatal(err)
	}
}
