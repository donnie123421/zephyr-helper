package truenas

import (
	"errors"
	"fmt"
	"testing"
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
