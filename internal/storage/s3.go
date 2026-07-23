package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 is object storage in a real bucket.
//
// It is the only place in this repository that holds a credential, and that is
// the whole architecture in one sentence: the credential lives on the host, in
// a package the guest cannot reach, behind an interface that only ever sees
// keys already confined to a sandbox's prefix.
type S3 struct {
	client *s3.Client
	bucket string
}

// S3Config configures the bucket.
type S3Config struct {
	Bucket string

	// Region, or empty to take it from the environment.
	Region string

	// Endpoint overrides the AWS endpoint, for MinIO, R2, or a test double.
	Endpoint string

	// UsePathStyle addresses buckets as endpoint/bucket rather than
	// bucket.endpoint. Required by MinIO and most S3-compatible servers, since
	// virtual-host addressing needs wildcard DNS they do not have.
	UsePathStyle bool
}

// NewS3 connects to a bucket.
//
// Credentials come from the ambient chain -- environment, shared config, or the
// instance's role -- and are never passed through this API. A daemon flag
// holding a secret key is a secret key in a process listing and in whatever
// started the daemon.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: a bucket is required")
	}

	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage: load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})

	return &S3{client: client, bucket: cfg.Bucket}, nil
}

func (s *S3) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	// A ranged GET is what a filesystem read becomes. Without it, reading 4KB
	// of a 5GB object transfers 5GB and bills for 5GB.
	if offset > 0 || length >= 0 {
		in.Range = aws.String(byteRange(offset, length))
	}

	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, s.translate(err, key)
	}
	return out.Body, nil
}

// byteRange renders an HTTP range header. length < 0 means "to the end".
func byteRange(offset, length int64) string {
	if length < 0 {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	// HTTP ranges are inclusive at both ends, so the last byte is offset+length-1.
	// Off by one here reads one byte too many, forever, and nothing complains.
	return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
}

func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}

	if _, err := s.client.PutObject(ctx, in); err != nil {
		return s.translate(err, key)
	}
	return nil
}

func (s *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return ObjectInfo{}, s.translate(err, key)
	}

	info := ObjectInfo{Key: key}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	if out.ETag != nil {
		info.ETag = strings.Trim(*out.ETag, `"`)
	}
	return info, nil
}

func (s *S3) List(ctx context.Context, prefix, delimiter, cursor string, limit int) (Listing, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	in := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(int32(limit)),
	}
	if delimiter != "" {
		in.Delimiter = aws.String(delimiter)
	}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}

	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return Listing{}, s.translate(err, prefix)
	}

	listing := Listing{Objects: make([]ObjectInfo, 0, len(out.Contents))}
	for _, obj := range out.Contents {
		info := ObjectInfo{Key: aws.ToString(obj.Key)}
		if obj.Size != nil {
			info.Size = *obj.Size
		}
		if obj.LastModified != nil {
			info.LastModified = *obj.LastModified
		}
		if obj.ETag != nil {
			info.ETag = strings.Trim(*obj.ETag, `"`)
		}
		listing.Objects = append(listing.Objects, info)
	}
	for _, cp := range out.CommonPrefixes {
		listing.CommonPrefixes = append(listing.CommonPrefixes, aws.ToString(cp.Prefix))
	}

	// IsTruncated with no token would page forever; treat it as the end rather
	// than loop.
	if aws.ToBool(out.IsTruncated) && out.NextContinuationToken != nil {
		listing.Cursor = *out.NextContinuationToken
	}
	return listing, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return s.translate(err, key)
	}
	return nil
}

// translate maps an AWS error onto this package's.
//
// The layers above must not import the AWS SDK to find out whether something
// exists -- that is the whole point of the port. It also matters that a missing
// object is ErrNotFound rather than a wrapped API error: the FUSE layer turns
// that into ENOENT, and a program that stats a file that is not there is doing
// something completely normal.
func (s *S3) translate(err error, key string) error {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}

	// HeadObject reports a missing object as a bare 404 with no typed error,
	// which is an SDK wart rather than a distinction worth carrying upward.
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
	}
	return fmt.Errorf("storage: s3: %s: %w", key, err)
}

var _ Backend = (*S3)(nil)
