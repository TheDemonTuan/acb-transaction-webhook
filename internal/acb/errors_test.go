package acb

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestSanitizeTransportError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"reset", fmt.Errorf("post https://online.acb.com.vn/acbib/Request?secret=x: %w", syscall.ECONNRESET), ErrConnectionReset},
		{"timeout", context.DeadlineExceeded, ErrRequestTimeout},
		{"cancel", context.Canceled, ErrRequestCanceled},
		{"other", errors.New("cookie=secret"), ErrNetwork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeTransportError(tt.err); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestSafePathDropsQuery(t *testing.T) {
	if got := SafePath("https://online.acb.com.vn/acbib/Request?dse_sessionId=secret#fragment"); got != "/acbib/Request" {
		t.Fatalf("got %q", got)
	}
}
