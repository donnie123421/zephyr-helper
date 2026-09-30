package truenas

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReasonTrimsTheFraming(t *testing.T) {
	validation := &rpcError{Code: -32001, Message: "Method call error", Data: map[string]any{
		"reason": "[EINVAL] app_name: Application is already running\n",
	}}
	cases := []struct {
		err  error
		want string
	}{
		{validation, "Application is already running"},
		{fmt.Errorf("truenas app.start: %w", validation), "Application is already running"},
		{&rpcError{Code: -32001, Message: "Method call error", Data: map[string]any{"reason": "[EFAULT] Failed: pool is busy\nTraceback"}}, "Failed: pool is busy"},
		{&rpcError{Code: -32601, Message: "Method not found"}, "Method not found"},
		{errors.New("rpc connect: refused"), "rpc connect: refused"},
	}
	for _, c := range cases {
		if got := Reason(c.err); got != c.want {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if !IsMethodNotFound(fmt.Errorf("wrapped: %w", &rpcError{Code: -32601})) || IsMethodNotFound(validation) {
		t.Error("IsMethodNotFound misread the error code")
	}
}

func TestJobFailureReadsWell(t *testing.T) {
	cases := []struct {
		job  Job
		want string
	}{
		{Job{State: "FAILED", Error: "[EFAULT] Failed to pull image\nTraceback …"}, "Failed to pull image"},
		{Job{State: "ABORTED"}, "It was cancelled."},
		{Job{State: "FAILED"}, "TrueNAS didn't say why."},
	}
	for _, c := range cases {
		if got := c.job.Failure(); got != c.want {
			t.Errorf("Failure(%+v) = %q, want %q", c.job, got, c.want)
		}
	}
}

// A job TrueNAS stops listing has ended, and waiting on it stops at once
// rather than polling until the deadline.
func TestWaitingOnAJobTrueNASDroppedStopsAtOnce(t *testing.T) {
	fake := &fakeTrueNAS{loginSeen: make(chan struct{})}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := &Client{rpc: newRPCClient("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/current", "key", nil)}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	_, err := c.WaitJob(ctx, 42, time.Second)
	if !errors.Is(err, ErrJobGone) {
		t.Fatalf("WaitJob = %v, want ErrJobGone", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Errorf("took %s to give up on a job that's gone", time.Since(started))
	}
}
