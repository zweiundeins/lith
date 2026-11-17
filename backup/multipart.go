package backup

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
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
	newAPI      func(ctx context.Context) (multipartAPI, error)
	mu          sync.Mutex
	built       bool
	api         multipartAPI
	unsupported sync.Once // logs a provider without ListMultipartUploads
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
// client is built from, with its checksum policy: with an Endpoint, request
// checksums only where an operation requires one and no validation of
// response checksums, and a Content-MD5 on DeleteObjects.
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
		o.APIOptions = append(o.APIOptions, addDeleteContentMD5)
		if e := m.cfg.Endpoint; e != "" {
			if !strings.HasPrefix(e, "http://") && !strings.HasPrefix(e, "https://") {
				e = "https://" + e
			}
			o.BaseEndpoint = aws.String(e)
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
	}
	if region == "" {
		// S3 needs a region to answer at all: start from the host's, else
		// from us-east-1, as litestream does.
		look := cfg.Copy()
		if look.Region == "" {
			look.Region = "us-east-1"
		}
		if cfg.Region, err = bucketRegion(ctx, s3.NewFromConfig(look, endpoint), m.cfg.Bucket); err != nil {
			return nil, fmt.Errorf("s3 bucket region: %w", err)
		}
	}
	return s3.NewFromConfig(cfg, endpoint), nil
}

// addDeleteContentMD5 sets Content-MD5 on a DeleteObjects request, as
// litestream's client does (its RequireContentMD5): some providers refuse a
// DeleteObjects without it.
func addDeleteContentMD5(stack *middleware.Stack) error {
	return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("LithDeleteContentMD5",
		func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
			req, ok := in.Request.(*smithyhttp.Request)
			if !ok || middleware.GetOperationName(ctx) != "DeleteObjects" || req.Header.Get("Content-MD5") != "" {
				return next.HandleFinalize(ctx, in)
			}
			if body, ok := req.GetStream().(io.ReadSeeker); ok {
				start, err := body.Seek(0, io.SeekCurrent)
				if err != nil {
					return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf("content-md5: %w", err)
				}
				h := md5.New()
				if _, err := io.Copy(h, body); err != nil {
					return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf("content-md5: %w", err)
				}
				if _, err := body.Seek(start, io.SeekStart); err != nil {
					return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf("content-md5: %w", err)
				}
				req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(h.Sum(nil)))
			}
			return next.HandleFinalize(ctx, in)
		}), middleware.Before)
}

// bucketRegion asks S3 for a bucket's region with an unsigned HeadBucket.
// S3 names it in X-Amz-Bucket-Region in a 200, a 301 and a 403 alike.
func bucketRegion(ctx context.Context, client *s3.Client, bucket string) (string, error) {
	out, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}, func(o *s3.Options) {
		o.Credentials = nil
	})
	if err == nil {
		if r := aws.ToString(out.BucketRegion); r != "" {
			return r, nil
		}
		return "", fmt.Errorf("no region in the answer for bucket %s", bucket)
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		if r := re.Response.Header.Get("X-Amz-Bucket-Region"); r != "" {
			return r, nil
		}
	}
	return "", err
}

// maxUploadPages bounds a listing of unfinished uploads: a million at 1,000
// a page, more than any replica leaves.
const maxUploadPages = 1000

// s3KeyPrefix is the prefix of the keys litestream's S3 client writes for a
// replica, and for one of its levels when level is 0 or more: its Path, then
// the level in four hex digits (s3.ReplicaClient.WriteLTXFile). The file
// client's ltx/<level> layout does not apply.
func s3KeyPrefix(root string, level int) string {
	if level < 0 {
		return root + "/"
	}
	return root + "/" + fmt.Sprintf("%04x/", level)
}

// abortUploads aborts every unfinished multipart upload under the replica
// name, or under its level when level is 0 or more. It returns the number
// it aborted. A provider without ListMultipartUploads has none to abort.
func (m *Manager) abortUploads(ctx context.Context, name string, level int) (int, error) {
	api, err := m.multipart(ctx)
	if err != nil || api == nil {
		return 0, err
	}
	prefix := s3KeyPrefix(path.Join(m.cfg.Prefix, name), level)
	in := &s3.ListMultipartUploadsInput{Bucket: aws.String(m.cfg.Bucket), Prefix: aws.String(prefix)}
	aborted := 0
	var errs []error
	for page := 1; ; page++ {
		out, err := api.ListMultipartUploads(ctx, in)
		if notImplemented(err) {
			m.mpu.unsupported.Do(func() {
				m.log.Info("backup: the provider does not list multipart uploads; give the bucket a lifecycle rule that aborts them", "error", err)
			})
			return aborted, errors.Join(errs...)
		}
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
		if aws.ToString(out.NextKeyMarker) == aws.ToString(in.KeyMarker) && aws.ToString(out.NextUploadIdMarker) == aws.ToString(in.UploadIdMarker) {
			return aborted, errors.Join(append(errs, errors.New("list multipart uploads: the listing is truncated but does not move on"))...)
		}
		if page == maxUploadPages {
			return aborted, errors.Join(append(errs, fmt.Errorf("list multipart uploads: more than %d pages", maxUploadPages))...)
		}
		in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

// notImplemented reports an S3 error for an operation the provider lacks.
func notImplemented(err error) bool {
	if errorCode(err) == "NotImplemented" {
		return true
	}
	var status interface{ HTTPStatusCode() int }
	return errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotImplemented
}

// errorCode is the S3 error code of err, or "".
func errorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// abortCancelledSnapshot aborts what a snapshot cut off by its context may
// have left, with a fresh context, since the snapshot's has ended. It is best
// effort: a bucket lifecycle rule or PurgeReplica catches what it misses.
// The caller holds the replica's maint lock and, after a detach, the name's
// reservation, so no other snapshot of the name uploads meanwhile.
func (m *Manager) abortCancelledSnapshot(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), abortAfterCancel)
	defer cancel()
	if _, err := m.abortUploads(ctx, name, litestream.SnapshotLevel); err != nil {
		m.log.Warn("backup: the parts of a cancelled snapshot may be left in the bucket", "replica", name, "error", err)
	}
}
