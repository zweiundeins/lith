package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// A provider whose listings lag its writes, such as OpenStack Swift
// (Infomaniak), can leave an object written just before a purge out of the
// purge's listing, and go on listing a deleted one for a while.

// objectsAPI is the part of the S3 API that lists and deletes objects.
type objectsAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// purgeListings is how many times PurgeReplica lists an S3 replica after
// its deletes, Config.PurgeSettle apart, while a listing shows objects.
const purgeListings = 3

// settlePurge lists the replica's objects PurgeSettle after the purge's
// deletes, and again while a listing shows some, deleting what each shows.
// It fails when the last listing still shows objects.
func (m *Manager) settlePurge(ctx context.Context, name string) error {
	api, err := m.multipart(ctx)
	if err != nil {
		return err
	}
	objs, ok := api.(objectsAPI)
	if !ok {
		return nil // a Dir, whose listings are complete
	}
	bucket := aws.String(m.cfg.Bucket)
	prefix := s3KeyPrefix(path.Join(m.cfg.Prefix, name), -1)
	for i := 1; ; i++ {
		t := time.NewTimer(m.purgeSettle)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		keys, err := listKeys(ctx, objs, bucket, prefix)
		if err != nil || len(keys) == 0 {
			return err
		}
		if i == purgeListings {
			return fmt.Errorf("%d objects still listed %s after the deletes; purge again later", len(keys), time.Duration(purgeListings)*m.purgeSettle)
		}
		if err := deleteKeys(ctx, objs, bucket, keys); err != nil {
			return err
		}
		m.log.Info("backup: purge deleted objects that were listed after its deletes", "replica", name, "objects", len(keys))
	}
}

// listKeys lists every key under prefix.
func listKeys(ctx context.Context, api objectsAPI, bucket *string, prefix string) ([]string, error) {
	var keys []string
	err := listObjects(ctx, api, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String(prefix)}, func(out *s3.ListObjectsV2Output) {
		for _, o := range out.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	})
	return keys, err
}

// listObjects hands every page of the listing in to page, at most
// maxUploadPages of them.
func listObjects(ctx context.Context, api objectsAPI, in *s3.ListObjectsV2Input, page func(*s3.ListObjectsV2Output)) error {
	for n := 1; ; n++ {
		out, err := api.ListObjectsV2(ctx, in)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		page(out)
		if !aws.ToBool(out.IsTruncated) {
			return nil
		}
		if out.NextContinuationToken == nil || aws.ToString(out.NextContinuationToken) == aws.ToString(in.ContinuationToken) {
			return errors.New("list objects: the listing is truncated but does not move on")
		}
		if n == maxUploadPages {
			return fmt.Errorf("list objects: more than %d pages", maxUploadPages)
		}
		in.ContinuationToken = out.NextContinuationToken
	}
}

// ReplicaNames lists the names of the replicas the bucket (or Dir) holds
// under Config.Prefix, sorted, without the prefix: every name something was
// ever replicated under, attached or not, including a name whose
// PurgeReplica failed and left objects behind. An app that removes tenants
// compares it to its own account list and purges the names it no longer
// has. With no Prefix that is every top-level name in the bucket, also
// those of anything else stored there. Names PurgeReplica would refuse are
// left out. A disabled Manager returns nil.
func (m *Manager) ReplicaNames(ctx context.Context) ([]string, error) {
	if !m.enabled {
		return nil, nil
	}
	var names []string
	if m.cfg.Dir != "" {
		entries, err := os.ReadDir(filepath.Join(m.cfg.Dir, filepath.FromSlash(m.cfg.Prefix)))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("backup: replica names: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() && checkName(e.Name()) == nil {
				names = append(names, e.Name())
			}
		}
		return names, nil // ReadDir sorts by name
	}
	api, err := m.multipart(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: replica names: %w", err)
	}
	objs, ok := api.(objectsAPI)
	if !ok {
		return nil, errors.New("backup: replica names: the S3 client cannot list objects")
	}
	prefix := ""
	if m.cfg.Prefix != "" {
		prefix = m.cfg.Prefix + "/"
	}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(m.cfg.Bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/")}
	err = listObjects(ctx, objs, in, func(out *s3.ListObjectsV2Output) {
		for _, p := range out.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(p.Prefix), prefix), "/")
			if checkName(name) == nil {
				names = append(names, name)
			}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("backup: replica names: %w", err)
	}
	slices.Sort(names)
	return slices.Compact(names), nil // a provider may repeat a prefix on the next page, as gofakes3 does
}

// deleteKeys deletes keys, 1,000 a request. A key that is gone already
// counts as deleted.
func deleteKeys(ctx context.Context, api objectsAPI, bucket *string, keys []string) error {
	for len(keys) > 0 {
		n := min(len(keys), 1000)
		ids := make([]types.ObjectIdentifier, n)
		for i, k := range keys[:n] {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := api.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
		if err != nil {
			return fmt.Errorf("delete objects: %w", err)
		}
		var errs []error
		for _, e := range out.Errors {
			if aws.ToString(e.Code) != "NoSuchKey" {
				errs = append(errs, fmt.Errorf("delete %s: %s: %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
		keys = keys[n:]
	}
	return nil
}
