package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// S3WriteMount records an explicit destination and its publication deadline.
type S3WriteMount struct {
	Endpoint string
	Bucket   string
	Prefix   string
	Root     string
	Timeout  time.Duration
}

type mountedS3Writer struct {
	mount        mountedS3Reader
	timeout      time.Duration
	pollInterval time.Duration
}

func ValidateS3WriteMounts(mounts []S3WriteMount) error {
	roots := make([]S3ReadMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Timeout <= 0 {
			return fmt.Errorf("mounted S3 write timeout must be positive")
		}
		roots = append(roots, S3ReadMount{Endpoint: mount.Endpoint, Bucket: mount.Bucket, Prefix: mount.Prefix, Root: mount.Root})
	}
	return ValidateS3ReadMounts(roots)
}

func (s3 *GenericS3) mountedWriteURL(raw string) (string, bool) {
	if s3.mountedWrite == nil {
		return "", false
	}
	u, err := s3.parse(raw)
	if err != nil {
		return "", false
	}
	canonical := "s3://" + u.bucket + "/" + u.path
	return canonical, inputPrefix(strings.TrimSuffix(canonical, "/"), s3.mountedWrite.mount.prefix)
}

func (s3 *GenericS3) putMounted(ctx context.Context, raw, sourcePath string) (*Object, error) {
	ctx, cancel := context.WithTimeout(ctx, s3.mountedWrite.timeout)
	defer cancel()
	canonical, _ := s3.mountedWriteURL(raw)
	relative, err := s3.mountedWrite.mount.resolve(canonical)
	if err != nil {
		return nil, err
	}
	info, err := waitForLocalFile(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s3.mountedWrite.mount.root)
	if err != nil {
		return nil, fmt.Errorf("open mounted output root: %w", err)
	}
	defer root.Close()
	if !info.IsDir() {
		return s3.publishMountedFile(ctx, root, relative, raw, sourcePath)
	}
	result := &Object{URL: raw}
	err = filepath.WalkDir(sourcePath, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		suffix, err := filepath.Rel(sourcePath, name)
		if err != nil {
			return err
		}
		fileURL := strings.TrimSuffix(raw, "/") + "/" + filepath.ToSlash(suffix)
		canonical, _ := s3.mountedWriteURL(fileURL)
		destination, err := s3.mountedWrite.mount.resolve(canonical)
		if err != nil {
			return err
		}
		obj, err := s3.publishMountedFile(ctx, root, destination, fileURL, name)
		if err != nil {
			return err
		}
		result.Size += obj.Size
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s3 *GenericS3) publishMountedFile(ctx context.Context, root *os.Root, destination, raw, sourcePath string) (*Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return nil, err
	}
	if !sourceInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("mounted output source %q is not a regular file", sourcePath)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("mounted output source %q is not a regular file", sourcePath)
	}
	if err := root.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return nil, err
	}
	existing, err := root.Stat(destination)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if existing != nil && !existing.Mode().IsRegular() {
		return nil, fmt.Errorf("mounted output destination is not a regular file")
	}
	// Open without truncating: the task may already have written the destination file.
	target, err := root.OpenFile(destination, os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	targetInfo, err := target.Stat()
	if err != nil {
		target.Close()
		return nil, err
	}
	if !targetInfo.Mode().IsRegular() {
		target.Close()
		return nil, fmt.Errorf("mounted output destination is not a regular file")
	}
	if !os.SameFile(info, targetInfo) {
		if err := target.Truncate(0); err != nil {
			target.Close()
			return nil, err
		}
		size, copyErr := io.Copy(target, inputReader{ctx: ctx, source: source})
		if err := errors.Join(copyErr, target.Close(), ctx.Err()); err != nil {
			return nil, fmt.Errorf("copy mounted output: %w", err)
		}
		if size != info.Size() {
			return nil, fmt.Errorf("mounted output source size changed during copy")
		}
	} else if err := target.Close(); err != nil {
		return nil, err
	}
	return s3.waitForMountedOutput(ctx, raw, info.Size())
}

func (s3 *GenericS3) waitForMountedOutput(ctx context.Context, raw string, expected int64) (*Object, error) {
	u, err := s3.parse(raw)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("waiting for mounted output %s: %w", raw, err)
		}
		// Query S3 directly; GenericS3.Stat can read the mount instead.
		info, err := s3.client.StatObject(ctx, u.bucket, u.path, minio.StatObjectOptions{})
		if err != nil {
			response := minio.ToErrorResponse(err)
			if response.StatusCode != 404 && response.StatusCode != 429 && response.StatusCode < 500 {
				return nil, fmt.Errorf("checking mounted output %s: %w", raw, err)
			}
		} else if info.Size == expected {
			return &Object{URL: raw, Name: u.path, Size: info.Size, ETag: info.ETag, LastModified: info.LastModified}, nil
		}
		timer := time.NewTimer(s3.mountedWrite.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("waiting for mounted output %s: %w", raw, ctx.Err())
		case <-timer.C:
		}
	}
}
