// Package objectstore is the S3-compatible object-storage backend for raw
// document bytes (SPEC-04 §5, STORY-06.3, ADR-0042). It backs the documents
// Storage seam (upload writes) and the ingest_document worker's fetch (read-back)
// with a single client. Locally the backend is MinIO (docker-compose service
// `minio`); in production it is any S3-compatible store, reached with a per-tenant-
// agnostic platform credential (object keys are namespaced by tenant by the caller).
//
// It uses aws-sdk-go-v2/service/s3 (the aws-sdk-go-v2 core is already a
// dependency, so this reuses it rather than adding a second S3 client — ADR-0042).
// Path-style addressing is forced (MinIO does not do virtual-host buckets) and a
// custom endpoint is honoured.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Config is the object-storage connection. All fields except Region are required;
// New fails closed if any is empty (SPEC-09: no silent no-credentials client).
type Config struct {
	Endpoint  string // e.g. http://minio:9000 (compose) or http://localhost:9000 (host)
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string // default "us-east-1" (MinIO ignores it but the SDK requires one)
}

// Client is an S3-compatible object store scoped to one bucket. Safe for
// concurrent use.
type Client struct {
	s3     *s3.Client
	bucket string
}

// New builds a client and ensures the bucket exists. It validates the config
// before dialling, so a misconfiguration is a startup error, not a runtime
// surprise on the first upload.
func New(ctx context.Context, cfg Config) (*Client, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, errors.New("objectstore: endpoint is required")
	case cfg.Bucket == "":
		return nil, errors.New("objectstore: bucket is required")
	case cfg.AccessKey == "":
		return nil, errors.New("objectstore: access key is required")
	case cfg.SecretKey == "":
		return nil, errors.New("objectstore: secret key is required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	api := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: true, // MinIO / most self-hosted stores
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	})

	c := &Client{s3: api, bucket: cfg.Bucket}
	if err := c.ensureBucket(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ensureBucket creates the bucket if it does not already exist. An already-owned
// bucket is not an error (idempotent, so a restart is safe).
func (c *Client) ensureBucket(ctx context.Context) error {
	_, err := c.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.bucket)})
	if err == nil {
		return nil
	}
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &exists) {
		return nil
	}
	return fmt.Errorf("objectstore: ensure bucket %q: %w", c.bucket, err)
}

// Put stores the raw bytes under key with the given content type (documents.Storage).
// The body is buffered so the SigV4 signer has a seekable, length-known payload;
// upload sizes are bounded by the upload ceiling (FR-SRC-02), so this is memory-safe.
//
// ponytail: buffers the whole object in memory (bounded by MAX_UPLOAD_BYTES). For
// very large objects switch to feature/s3/manager.Uploader (streaming multipart).
func (c *Client) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("objectstore: read upload: %w", err)
	}
	in := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := c.s3.PutObject(ctx, in); err != nil {
		return fmt.Errorf("objectstore: put %q: %w", key, err)
	}
	return nil
}

// Get fetches the object stored under key. The caller closes the returned reader.
// It is the read-back the ingest_document worker uses to fetch the uploaded bytes.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %q: %w", key, err)
	}
	return out.Body, nil
}
