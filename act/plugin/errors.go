package plugin

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StatusCode walks the wrap chain and returns the first gRPC status code,
// or codes.Unknown if none is present.
func StatusCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if st, ok := status.FromError(cur); ok {
			return st.Code()
		}
	}
	return codes.Unknown
}

// IsRetryable reports whether err is worth retrying against the same plugin.
// ExecError is never retryable; it carries an exit code from user code.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var execErr *ExecError
	if errors.As(err, &execErr) {
		return false
	}
	switch StatusCode(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted:
		return true
	default:
		return false
	}
}
