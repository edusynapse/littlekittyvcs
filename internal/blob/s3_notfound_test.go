package blob

import (
	"errors"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// wrapAPI builds the error chain GetObject returns: an operation error around
// the HTTP response error around the deserialized API error. The host-id
// wrapper only forwards errors.As into this chain.
func wrapAPI(status int, inner error) error {
	return &smithy.OperationError{
		ServiceID:     "S3",
		OperationName: "GetObject",
		Err: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{
					Response: &http.Response{StatusCode: status},
				},
				Err: inner,
			},
		},
	}
}

func TestNoSuchBucketIsNotMissingKey(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		missing bool
	}{
		{
			name: "get object code",
			err: wrapAPI(http.StatusNotFound, &smithy.GenericAPIError{
				Code:    "NoSuchBucket",
				Message: "The specified bucket does not exist",
			}),
		},
		{
			name: "typed no such bucket",
			err:  wrapAPI(http.StatusNotFound, &types.NoSuchBucket{}),
		},
		{
			name:    "typed no such key",
			err:     wrapAPI(http.StatusNotFound, &types.NoSuchKey{}),
			missing: true,
		},
		{
			name: "generic no such key",
			err: wrapAPI(http.StatusNotFound, &smithy.GenericAPIError{
				Code:    "NoSuchKey",
				Message: "The specified key does not exist",
			}),
			missing: true,
		},
		{
			name: "head status code",
			err: wrapAPI(http.StatusNotFound, &smithy.GenericAPIError{
				Code:    "NotFound",
				Message: "Not Found",
			}),
			missing: true,
		},
		{
			name:    "typed not found",
			err:     wrapAPI(http.StatusNotFound, &types.NotFound{}),
			missing: true,
		},
		{
			name:    "bodyless 404",
			err:     wrapAPI(http.StatusNotFound, errors.New("empty head")),
			missing: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := asNotFound("k", tc.err)
			if tc.missing {
				if !errors.Is(got, ErrNotFound) {
					t.Fatalf("got %v", got)
				}
				return
			}
			if got != tc.err {
				t.Fatalf("got %v", got)
			}
			if errors.Is(got, ErrNotFound) {
				t.Fatalf("mapped to not found: %v", got)
			}
		})
	}
}
