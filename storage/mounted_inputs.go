package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ohsu-comp-bio/funnel/config"
)

// MountedInputs overrides reads for explicit mounts. Writes and URL joins always
// use the configured storage backend, so local files cannot prove publication.
type MountedInputs struct {
	Storage
	mounts []inputMount
}

type inputMount struct {
	prefix string
	root   string
}

// WithMountedInputs configures worker-local reads without changing server storage.
// Mount directories are opened on access, after Kubernetes has mounted them.
func WithMountedInputs(backend Storage, mappings []*config.MountedInput) (*MountedInputs, error) {
	if backend == nil {
		return nil, errors.New("mounted inputs require a fallback storage backend")
	}
	s := &MountedInputs{Storage: backend}
	for _, mapping := range mappings {
		if mapping == nil {
			return nil, errors.New("nil mounted input mapping")
		}
		if err := validateInputURL(mapping.URLPrefix); err != nil {
			return nil, fmt.Errorf("mounted input prefix: %w", err)
		}
		if !filepath.IsAbs(mapping.Path) || filepath.Clean(mapping.Path) != mapping.Path {
			return nil, fmt.Errorf("mounted input path %q must be absolute and clean", mapping.Path)
		}
		prefix := strings.TrimSuffix(mapping.URLPrefix, "/")
		for _, existing := range s.mounts {
			if inputPrefix(prefix, existing.prefix) || inputPrefix(existing.prefix, prefix) {
				return nil, fmt.Errorf("overlapping mounted input prefixes %q and %q", mapping.URLPrefix, existing.prefix)
			}
		}
		s.mounts = append(s.mounts, inputMount{prefix: prefix, root: mapping.Path})
	}
	return s, nil
}

// Funnel's S3 backends use literal keys, including spaces, #, and percent signs.
// Do not URI-decode keys into different filesystem names.
func validateInputURL(raw string) error {
	rest, ok := strings.CutPrefix(raw, "s3://")
	host, key, _ := strings.Cut(rest, "/")
	if !ok || host == "" || strings.ContainsAny(host, "@?#\\ \x00") {
		return fmt.Errorf("invalid mounted S3 input prefix %q", raw)
	}
	for _, value := range []string{key, decodedInputKey(key)} {
		for _, component := range strings.Split(value, "/") {
			if component == "." || component == ".." || strings.ContainsAny(component, "\\\x00") {
				return fmt.Errorf("unsafe mounted input URL %q", raw)
			}
		}
		if strings.Contains(value, "//") {
			return fmt.Errorf("ambiguous mounted input path %q", raw)
		}
	}
	return nil
}

func decodedInputKey(key string) string {
	decoded, err := url.PathUnescape(key)
	if err != nil {
		return key
	}
	return decoded
}

func inputPrefix(value, prefix string) bool {
	return value == prefix || strings.HasPrefix(value, prefix+"/")
}

func (s *MountedInputs) resolve(raw string) (*inputMount, string, error) {
	for i := range s.mounts {
		m := &s.mounts[i]
		if !inputPrefix(strings.TrimSuffix(raw, "/"), m.prefix) {
			continue
		}
		if err := validateInputURL(raw); err != nil {
			return nil, "", err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(raw, m.prefix), "/")
		if rel == "" {
			rel = "."
		}
		return m, strings.TrimSuffix(rel, "/"), nil
	}
	return nil, "", nil
}

func mountedObject(raw string, info fs.FileInfo) *Object {
	return &Object{URL: raw, Name: info.Name(), Size: info.Size(), LastModified: info.ModTime()}
}

func (s *MountedInputs) Stat(ctx context.Context, raw string) (*Object, error) {
	m, rel, err := s.resolve(raw)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return s.Storage.Stat(ctx, raw)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return nil, fmt.Errorf("open mounted input root: %w", err)
	}
	defer root.Close() // Read-only directory handle; closing cannot affect copied data.
	info, err := root.Stat(rel)
	if err != nil {
		return nil, fmt.Errorf("stat mounted input: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mounted input %q is not a regular file or directory", raw)
	}
	return mountedObject(raw, info), nil
}

func (s *MountedInputs) List(ctx context.Context, raw string) ([]*Object, error) {
	m, rel, err := s.resolve(raw)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return s.Storage.List(ctx, raw)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return nil, fmt.Errorf("open mounted input root: %w", err)
	}
	defer root.Close() // Read-only directory handle.
	info, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("mounted input %q is not a directory", raw)
	}
	var objects []*Object
	err = fs.WalkDir(root.FS(), rel, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := root.Stat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("mounted directory entry %q is not a regular file", name)
		}
		fileURL := m.prefix + "/" + name
		objects = append(objects, mountedObject(fileURL, info))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list mounted input: %w", err)
	}
	return objects, nil
}

func (s *MountedInputs) Get(ctx context.Context, raw, destination string) (*Object, error) {
	m, rel, err := s.resolve(raw)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return s.Storage.Get(ctx, raw, destination)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return nil, fmt.Errorf("open mounted input root: %w", err)
	}
	defer root.Close() // Read-only directory handle.
	info, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mounted input %q is not a regular file", raw)
	}
	source, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("open mounted input: %w", err)
	}
	defer source.Close() // Read-only source; destination Close is checked below.
	info, err = source.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mounted input %q is not a regular file", raw)
	}
	targetInfo, err := os.Stat(destination)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if targetInfo != nil && os.SameFile(info, targetInfo) {
		return nil, errors.New("mounted input source and destination must differ")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return nil, err
	}
	target, err := os.Create(destination)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(target, inputReader{ctx: ctx, source: source})
	if err := errors.Join(copyErr, target.Close(), ctx.Err()); err != nil {
		return nil, fmt.Errorf("copy mounted input: %w", err)
	}
	return mountedObject(raw, info), nil
}

type inputReader struct {
	ctx    context.Context
	source io.Reader
}

func (r inputReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func (s *MountedInputs) UnsupportedOperations(raw string) UnsupportedOperations {
	unsupported := s.Storage.UnsupportedOperations(raw)
	m, _, err := s.resolve(raw)
	if err != nil {
		unsupported.Get, unsupported.Stat, unsupported.List = err, err, err
		return unsupported
	}
	if m != nil {
		unsupported.Get, unsupported.Stat, unsupported.List = nil, nil, nil
	}
	return unsupported
}
