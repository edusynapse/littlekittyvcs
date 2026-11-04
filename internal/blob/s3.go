package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config is the bucket connection. This package does not read the environment.
// Credentials are the static keys in this struct. The default credential chain
// is not used, so a developer's ~/.aws keys are never sent to the endpoint.
type S3Config struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey string
	PathStyle                                      bool
}

// S3 is a bucket client.
type S3 struct {
	client *s3.Client
	bucket string
}

// NewS3 builds a path-style or virtual-hosted client.
// Request and response checksums are calculated only when the operation
// requires them. The SDK default sends a CRC checksum on every upload, and
// some S3-compatible services reject that header.
func NewS3(ctx context.Context, c S3Config) (*S3, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := aws.Config{
		Region:                     c.Region,
		BaseEndpoint:               aws.String(c.Endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(c.Endpoint)
		o.UsePathStyle = c.PathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3{client: client, bucket: c.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size < 0 {
		return errors.New("blob: negative size")
	}
	if size <= SinglePutMax {
		buf, err := readExactly(r, size)
		if err != nil {
			return err
		}
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(buf),
			ContentLength: aws.Int64(size),
		})
		return err
	}
	parts, err := planParts(size)
	if err != nil {
		return err
	}
	return s.putMultipart(ctx, key, r, parts)
}

func (s *S3) putMultipart(ctx context.Context, key string, r io.Reader, parts []part) error {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	uploadID := aws.ToString(created.UploadId)
	if uploadID == "" {
		return errors.New("blob: multipart upload id missing")
	}

	err = s.uploadParts(ctx, key, uploadID, r, parts)
	if err != nil {
		s.abort(key, uploadID)
		return err
	}
	return nil
}

func (s *S3) uploadParts(ctx context.Context, key, uploadID string, r io.Reader, parts []part) error {
	completed := make([]types.CompletedPart, 0, len(parts))
	for _, p := range parts {
		buf, err := readN(r, p.Size)
		if err != nil {
			return err
		}
		out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(p.Number),
			Body:          bytes.NewReader(buf),
			ContentLength: aws.Int64(p.Size),
		})
		if err != nil {
			return err
		}
		completed = append(completed, types.CompletedPart{
			ETag:       out.ETag,
			PartNumber: aws.Int32(p.Number),
		})
	}
	if err := ensureEOF(r); err != nil {
		return err
	}
	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	return err
}

// abort uses a fresh context so a cancelled upload context still aborts the upload.
// The original error is what the caller sees.
func (s *S3) abort(key, uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
}

func (s *S3) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if off < 0 || n <= 0 {
		return nil, fmt.Errorf("blob: invalid range %d+%d", off, n)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, off+n-1)),
	})
	if err != nil {
		return nil, asNotFound(key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, n+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != n {
		return nil, fmt.Errorf("blob: range read got %d bytes, want %d", len(data), n)
	}
	return data, nil
}

func (s *S3) Head(ctx context.Context, key string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return Info{}, asNotFound(key, err)
	}
	return Info{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified),
	}, nil
}

func (s *S3) List(ctx context.Context, prefix string, fn func(Info) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			err := fn(Info{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.Head(ctx, key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return asNotFound(key, err)
	}
	return nil
}

func asNotFound(key string, err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	return err
}

// isNotFound reports a missing key. NoSuchBucket is a missing bucket: S3
// answers that with HTTP 404 as well, and that error is returned unchanged.
// A HEAD response has no body, so a body-less 404 still cannot be told apart
// from a missing key and stays ErrNotFound.
func isNotFound(err error) bool {
	var noSuchBucket *types.NoSuchBucket
	if errors.As(err, &noSuchBucket) {
		return false
	}
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "NoSuchBucket" {
		return false
	}
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}
