package plugin

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStatusCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"plain", errors.New("boom"), codes.Unknown},
		{"unwrapped status", status.Error(codes.Unavailable, "down"), codes.Unavailable},
		{"single-wrap", fmt.Errorf("plugin foo: %w", status.Error(codes.NotFound, "x")), codes.NotFound},
		{"double-wrap", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", status.Error(codes.Aborted, "x"))), codes.Aborted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, StatusCode(tt.err))
		})
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unavailable", fmt.Errorf("plugin: %w", status.Error(codes.Unavailable, "down")), true},
		{"deadline", fmt.Errorf("plugin: %w", status.Error(codes.DeadlineExceeded, "slow")), true},
		{"aborted", fmt.Errorf("plugin: %w", status.Error(codes.Aborted, "txn")), true},
		{"not_found", fmt.Errorf("plugin: %w", status.Error(codes.NotFound, "x")), false},
		{"failed_precondition", fmt.Errorf("plugin: %w", status.Error(codes.FailedPrecondition, "x")), false},
		{"plain error", errors.New("boom"), false},
		{"exec error", &ExecError{ExitCode: 1, Message: "bad"}, false},
		{"wrapped exec error masking unavailable", fmt.Errorf("wrap: %w", &ExecError{ExitCode: 1}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsRetryable(tt.err))
		})
	}
}
