package s3

import (
	"errors"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// stringAPIError is a minimal smithy.APIError used to drive isNotFound /
// isNoSuchBucket through their string-based ErrorCode path. The typed
// branch (NoSuchKey / NoSuchBucket) is already exercised separately
// using the SDK's generated types.
type stringAPIError struct {
	code, msg string
}

func (e *stringAPIError) Error() string                          { return e.code + ": " + e.msg }
func (e *stringAPIError) ErrorCode() string                      { return e.code }
func (e *stringAPIError) ErrorMessage() string                   { return e.msg }
func (e *stringAPIError) ErrorFault() smithy.ErrorFault          { return smithy.FaultClient }
func (e *stringAPIError) Unwrap() error                          { return nil }
func (e *stringAPIError) RetryAfterSeconds() (uint, bool, error) { return 0, false, nil }

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "typed NoSuchKey", err: &s3types.NoSuchKey{}, want: true},
		{name: "string NoSuchKey", err: &stringAPIError{code: "NoSuchKey"}, want: true},
		{name: "string NotFound", err: &stringAPIError{code: "NotFound"}, want: true},
		{name: "other API error", err: &stringAPIError{code: "AccessDenied"}, want: false},
		{name: "plain error", err: errors.New("nope"), want: false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFound(tt.err); got != tt.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsNoSuchBucket(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "typed NoSuchBucket", err: &s3types.NoSuchBucket{}, want: true},
		{name: "string NoSuchBucket", err: &stringAPIError{code: "NoSuchBucket"}, want: true},
		{name: "other API error", err: &stringAPIError{code: "NoSuchKey"}, want: false},
		{name: "plain error", err: errors.New("nope"), want: false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoSuchBucket(tt.err); got != tt.want {
				t.Errorf("isNoSuchBucket(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
