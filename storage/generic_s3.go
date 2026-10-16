package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/calypr/funnel/config"
	"github.com/calypr/funnel/logger"
	s3util "github.com/calypr/funnel/util/s3"
)

// GenericS3 provides access to an S3-compatible object store (MinIO, Ceph,
// OpenStack Swift S3 API, etc). It uses the AWS S3 client configured with a
// custom endpoint and path-style addressing.
type GenericS3 struct {
	client   *s3.Client
	endpoint string
	kmskeyId string
}

// NewGenericS3 creates a new GenericS3 instance, given an endpoint URL
// and a set of authentication credentials.
func NewGenericS3(conf *config.GenericS3Storage) (*GenericS3, error) {

	var endpoint string
	if conf.Endpoint != "" {
		endpoint = s3util.ParseEndpoint(conf.Endpoint)
	}

	region := conf.Region
	if region == "" {
		region = "us-east-1"
	}

	logger := logger.NewLogger("GenericS3", logger.DefaultConfig())
	logger.Debug("generics3: connecting to endpoint", "endpoint", endpoint)

	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.UsePathStyle = true
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		},
	}
	if endpoint != "" {
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(schemeEndpoint(conf.Endpoint, endpoint))
		})
	}
	loadOpts := []func(*awscfg.LoadOptions) error{
		awscfg.WithRegion(region),
	}
	if conf.Key != "" && conf.Secret != "" {
		loadOpts = append(loadOpts,
			awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(conf.Key, conf.Secret, "")))
	}
	awsConfig, err := awscfg.LoadDefaultConfig(context.Background(), loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("error creating generic s3 backend: %v", err)
	}

	client := s3.NewFromConfig(awsConfig, opts...)

	return &GenericS3{client, endpoint + "/", conf.KmsKeyID}, nil
}

// schemeEndpoint rebuilds a full endpoint URL (with scheme) from the raw
// configured endpoint and the parsed host.
func schemeEndpoint(rawEndpoint, host string) string {
	if strings.HasPrefix(rawEndpoint, "https://") {
		return "https://" + host
	}
	return "http://" + host
}

// isDir returns true if a remote S3 object is a directory, false otherwise
func (s3b *GenericS3) isDir(ctx context.Context, bucketName, objectName string) (bool, error) {
	// Check if the objectName ends with '/' - often used to represent 'folders'
	if strings.HasSuffix(objectName, "/") {
		return true, nil
	}

	// List objects with the prefix to see if there are multiple keys with the given prefix
	pages := s3.NewListObjectsV2Paginator(s3b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucketName),
		Prefix: aws.String(objectName),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return false, err
		}
		for _, obj := range page.Contents {
			// If any object's key starts with the objectName and is not equal, it's a directory
			if strings.HasPrefix(aws.ToString(obj.Key), objectName) && obj.Key != aws.String(objectName) {
				return true, nil
			}
		}
	}

	// If no objects share the prefix or the objectName exactly matches a key, it's considered a file
	return false, nil
}

// Stat returns information about the object at the given storage URL.
func (s3b *GenericS3) Stat(ctx context.Context, url string) (*Object, error) {
	logger := logger.NewLogger("GenericS3", logger.DefaultConfig())
	u, err := s3b.parse(url)
	if err != nil {
		return nil, err
	}

	logger.Debug("genericS3: s3.client.HeadObject", "bucket", u.bucket, "path", u.path)
	res, err := s3b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(u.path),
	})
	if err != nil {
		logger.Debug("genericS3: getting object from s3.client.HeadObject", "url", url, "error", err)
		// the key may represent a directory prefix rather than an object
		isDir, derr := s3b.isDir(ctx, u.bucket, u.path)
		if derr == nil && isDir {
			return &Object{
				URL:  url,
				Name: u.path,
				Size: 0,
			}, nil
		}
		return nil, fmt.Errorf("genericS3: stat object %s in bucket %s: %s", u.path, u.bucket, err)
	}

	return &Object{
		URL:          url,
		Name:         u.path,
		ETag:         aws.ToString(res.ETag),
		LastModified: aws.ToTime(res.LastModified),
		Size:         aws.ToInt64(res.ContentLength),
	}, nil
}

// List lists the objects at the given url.
func (s3b *GenericS3) List(ctx context.Context, url string) ([]*Object, error) {
	u, err := s3b.parse(url)
	if err != nil {
		return nil, err
	}

	// Recursively list all objects.
	var objects []*Object
	pages := s3.NewListObjectsV2Paginator(s3b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(u.bucket),
		Prefix: aws.String(u.path),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, info := range page.Contents {
			// check if key represents a directory
			if strings.HasSuffix(aws.ToString(info.Key), "/") {
				continue
			}
			objects = append(objects, &Object{
				URL:          "s3://" + u.bucket + "/" + aws.ToString(info.Key),
				Name:         aws.ToString(info.Key),
				ETag:         aws.ToString(info.ETag),
				LastModified: aws.ToTime(info.LastModified),
				Size:         aws.ToInt64(info.Size),
			})
		}
	}

	return objects, nil
}

// Get copies an object from S3 to the host path.
func (s3b *GenericS3) Get(ctx context.Context, url, path string) (*Object, error) {
	logger := logger.NewLogger("GenericS3", logger.DefaultConfig())
	logger.Debug("genericS3: Get called", "url", url, "path", path)

	obj, err := s3b.Stat(ctx, url)
	if err != nil {
		return nil, err
	}

	u, err := s3b.parse(url)
	if err != nil {
		return nil, err
	}

	isDir, err := s3b.isDir(ctx, u.bucket, u.path)
	if err != nil {
		logger.Debug("genericS3: checking directory", "bucket", u.bucket, "path", u.path)
		return nil, fmt.Errorf("genericS3: getting object from isDir %s: %v", url, err)
	}
	if isDir {
		objects, err := s3b.List(ctx, url)
		if err != nil {
			return nil, err
		}

		for _, obj := range objects {
			relPath, err := filepath.Rel(u.path, obj.Name)
			if err != nil {
				return nil, fmt.Errorf("genericS3: computing relative path for %s: %v", obj.Name, err)
			}
			err = s3b.download(ctx, u.bucket, obj.Name, filepath.Join(path, relPath))
			if err != nil {
				return nil, err
			}
		}
	} else {
		err = s3b.download(ctx, u.bucket, u.path, path)
		if err != nil {
			return nil, fmt.Errorf("genericS3: getting object from download %s: %v", url, err)
		}
	}

	return obj, nil
}

// download streams an object to a file without using os.Rename
func (s3b *GenericS3) download(ctx context.Context, bucket, objectPath, filePath string) error {
	logger := logger.NewLogger("GenericS3", logger.DefaultConfig())

	logger.Debug("genericS3: downloading object", "bucket", bucket, "objectPath", objectPath, "filePath", filePath)
	// Step 1: Get the object stream
	res, err := s3b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objectPath),
	})
	if err != nil {
		return fmt.Errorf("failed getting object from S3: %w", err)
	}
	defer res.Body.Close()

	// Step 2: Create the local file (overwrite if exists)
	logger.Debug("genericS3: creating local file", "filePath", filePath)
	dir := filepath.Dir(filePath)
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return fmt.Errorf("failed creating directories: %w", err)
	}

	outFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed creating file: %w", err)
	}

	// Step 3: Copy the contents
	logger.Debug("genericS3: io.Copy", "outFile", outFile.Name(), "bucket", bucket)
	if _, err := io.Copy(outFile, res.Body); err != nil {
		outFile.Close()
		return fmt.Errorf("failed writing file: %w", err)
	}

	// Close explicitly (not via defer) so Mountpoint begins flushing to S3
	// before we poll for readability below.
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("failed closing file: %w", err)
	}
	return waitReadable(filePath, logger)
}

// waitReadable polls until the file at path is readable. When the path is on a
// Mountpoint-for-S3 backed filesystem, closing the write handle triggers an
// async S3 upload; os.Stat returns EPERM until that upload finishes.
func waitReadable(path string, logger *logger.Logger) error {
	const mountpointFlushTimeout = 5 * time.Minute
	const mountpointFlushInterval = 2 * time.Second
	deadline := time.Now().Add(mountpointFlushTimeout)
	for {
		_, err := os.Stat(path)
		if err == nil {
			break
		}
		if time.Now().Before(deadline) && (errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrNotExist)) {
			logger.Debug("genericS3: waiting for file to be readable after write", "path", path, "error", err)
			time.Sleep(mountpointFlushInterval)
			continue
		}
		return fmt.Errorf("failed waiting for file to become readable: %w", err)
	}
	return nil
}

// Put copies an object (file) from the host path to S3.
// Update Put function to be able to upload directories (a la Get() function)
func (s3b *GenericS3) Put(ctx context.Context, url, path string) (*Object, error) {
	logger := logger.NewLogger("GenericS3", logger.DefaultConfig())

	u, err := s3b.parse(url)
	if err != nil {
		return nil, err
	}

	// Wait for the local path to become readable. When the local path is on a
	// Mountpoint-for-S3 backed filesystem and the file was written by another
	// mount instance (the executor pod), os.Stat returns EPERM until Mountpoint
	// finishes flushing the write to S3. Poll until the file is readable or the
	// timeout expires.
	const mountpointFlushTimeout = 5 * time.Minute
	const mountpointFlushInterval = 2 * time.Second
	deadline := time.Now().Add(mountpointFlushTimeout)
	var fileInfo os.FileInfo
	for {
		var err error
		fileInfo, err = os.Stat(path)
		if err == nil {
			break
		}
		if time.Now().Before(deadline) && (errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrNotExist)) {
			logger.Debug("genericS3: waiting for output file to become readable", "path", path, "error", err)
			time.Sleep(mountpointFlushInterval)
			continue
		}
		return nil, fmt.Errorf("genericS3: putting object %s: %v", url, err)
	}

	// Check if the path is a directory
	if fileInfo.IsDir() {
		// Walk the directory and upload all files and subdirectories
		err = filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				// Upload the file
				relativePath, err := filepath.Rel(path, filePath)
				if err != nil {
					return err
				}
				uploadPath := filepath.ToSlash(filepath.Join(u.path, relativePath))
				err = s3b.upload(ctx, u.bucket, uploadPath, filePath)
				if err != nil {
					return fmt.Errorf("genericS3: putting nested object %s: %v", url, err)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		err = s3b.upload(ctx, u.bucket, u.path, path)
		if err != nil {
			return nil, fmt.Errorf("genericS3: putting object %s: %v", url, err)
		}
	}

	obj, err := s3b.Stat(ctx, url)

	return obj, err
}

// upload copies a local file to the bucket at the given key.
func (s3b *GenericS3) upload(ctx context.Context, bucket, key, filePath string) error {
	logger := logger.NewLogger("GenericS3", logger.DefaultConfig())

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	in := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   f,
	}
	if s3b.kmskeyId != "" {
		logger.Debug("genericS3: using KMS encryption for upload", "kmsKeyId", s3b.kmskeyId)
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s3b.kmskeyId)
	}
	_, err = s3b.client.PutObject(ctx, in)
	return err
}

// Join joins the given URL with the given subpath.
func (s3b *GenericS3) Join(url, path string) (string, error) {
	return strings.TrimSuffix(url, "/") + "/" + path, nil
}

// UnsupportedOperations describes which operations (Get, Put, etc) are not
// supported for the given URL.
func (s3b *GenericS3) UnsupportedOperations(url string) UnsupportedOperations {
	u, err := s3b.parse(url)
	if err != nil {
		return AllUnsupported(err)
	}
	_, err = s3b.client.HeadBucket(context.Background(), &s3.HeadBucketInput{
		Bucket: aws.String(u.bucket),
	})
	if err != nil {
		err = fmt.Errorf("genericS3: failed to find bucket %q: %v", u.bucket, err)
		return AllUnsupported(err)
	}
	return AllSupported()
}

func (s3b *GenericS3) parse(rawurl string) (*urlparts, error) {
	if !strings.HasPrefix(rawurl, s3Protocol) {
		return nil, &ErrUnsupportedProtocol{"genericS3"}
	}

	path := strings.TrimPrefix(rawurl, s3Protocol)
	path = strings.TrimPrefix(path, s3b.endpoint)
	if path == "" {
		return nil, &ErrInvalidURL{"genericS3"}
	}

	split := strings.SplitN(path, "/", 2)
	url := &urlparts{}
	if len(split) > 0 {
		url.bucket = split[0]
	}
	if len(split) == 2 {
		url.path = split[1]
	}
	return url, nil
}
