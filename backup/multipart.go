package backup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/benbjohnson/litestream"
)

// An upload of 5 MiB or more goes up in parts. One cut off by a cancelled
// context leaves its parts in the bucket: the SDK aborts it with the same,
// cancelled context, and the parts never show in a listing of objects, so
// DeleteAll leaves them. These find and abort such uploads.

// multipartAPI is the part of the S3 API that finds and aborts unfinished
// multipart uploads.
type multipartAPI interface {
	ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, opts ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// abortAfterCancel bounds the abort of the uploads a cancelled snapshot left.
const abortAfterCancel = 10 * time.Second

// multipartClient builds the Manager's S3 client for multipart uploads with
// newAPI, once it succeeds. The API is nil for a Dir.
type multipartClient struct {
	newAPI func(ctx context.Context) (multipartAPI, error)
	mu     sync.Mutex
	built  bool
	api    multipartAPI
}

func (m *Manager) multipart(ctx context.Context) (multipartAPI, error) {
	m.mpu.mu.Lock()
	defer m.mpu.mu.Unlock()
	if !m.mpu.built {
		api, err := m.mpu.newAPI(ctx)
		if err != nil {
			return nil, err
		}
		m.mpu.api, m.mpu.built = api, true
	}
	return m.mpu.api, nil
}

// newMultipartAPI builds an S3 client from the configuration litestream's
// client is built from.
func (m *Manager) newMultipartAPI(ctx context.Context) (multipartAPI, error) {
	if m.cfg.Bucket == "" {
		return nil, nil
	}
	region := m.cfg.Region
	if region == "" && m.cfg.Endpoint != "" {
		region = "us-east-1"
	}
	opts := []func(*config.LoadOptions) error{config.WithHTTPClient(&http.Client{Timeout: time.Minute})}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	if m.cfg.AccessKeyID != "" && m.cfg.SecretAccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(m.cfg.AccessKeyID, m.cfg.SecretAccessKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3 config: %w", err)
	}
	endpoint := func(o *s3.Options) {
		o.UsePathStyle = m.cfg.ForcePathStyle
		if e := m.cfg.Endpoint; e != "" {
			if !strings.HasPrefix(e, "http://") && !strings.HasPrefix(e, "https://") {
				e = "https://" + e
			}
			o.BaseEndpoint = aws.String(e)
		}
	}
	client := s3.NewFromConfig(cfg, endpoint)
	if region == "" {
		if cfg.Region, err = manager.GetBucketRegion(ctx, client, m.cfg.Bucket); err != nil {
			return nil, fmt.Errorf("s3 bucket region: %w", err)
		}
		client = s3.NewFromConfig(cfg, endpoint)
	}
	return client, nil
}

// abortUploads aborts every unfinished multipart upload under the replica
// name, or under its level when level is 0 or more. It returns the number
// it aborted.
func (m *Manager) abortUploads(ctx context.Context, name string, level int) (int, error) {
	api, err := m.multipart(ctx)
	if err != nil || api == nil {
		return 0, err
	}
	prefix := path.Join(m.cfg.Prefix, name)
	if level >= 0 {
		prefix = litestream.LTXLevelDir(prefix, level)
	}
	prefix += "/"
	in := &s3.ListMultipartUploadsInput{Bucket: aws.String(m.cfg.Bucket), Prefix: aws.String(prefix)}
	aborted := 0
	var errs []error
	for {
		out, err := api.ListMultipartUploads(ctx, in)
		if err != nil {
			return aborted, errors.Join(append(errs, fmt.Errorf("list multipart uploads: %w", err))...)
		}
		for _, u := range out.Uploads {
			_, err := api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: in.Bucket, Key: u.Key, UploadId: u.UploadId})
			var gone *types.NoSuchUpload
			switch {
			case err == nil:
				aborted++
			case !errors.As(err, &gone):
				errs = append(errs, fmt.Errorf("abort multipart upload %s: %w", aws.ToString(u.Key), err))
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			return aborted, errors.Join(errs...)
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// abortCancelledSnapshot aborts what a snapshot cancelled by detach may have
// left, with a fresh context, since the snapshot's is cancelled. It is best
// effort: a bucket lifecycle rule or PurgeReplica catches what it misses.
func (m *Manager) abortCancelledSnapshot(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortAfterCancel)
	defer cancel()
	if _, err := m.abortUploads(ctx, name, litestream.SnapshotLevel); err != nil {
		m.log.Warn("backup: the parts of a cancelled snapshot may be left in the bucket", "replica", name, "error", err)
	}
}
