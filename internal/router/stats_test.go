package router

import (
	"errors"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// When the container's usage can't be read, docker stats answers with an
// error, not a 200 without a body: the status is sent only once the first
// sample is read.
func TestStatsFailsBeforeAnswering(t *testing.T) {
	h, client := newTestRouter()
	if rec := do(t, h, "POST", "/containers/create?name=web", `{"Image":"nginx"}`); rec.Code != 201 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}

	// The Deployment reads one inspect makes; stats inspects the container
	// first, then reads its usage.
	var gets, allowed atomic.Int64
	allowed.Store(-1)
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if n := gets.Add(1); allowed.Load() >= 0 && n > allowed.Load() {
			return true, nil, errors.New("the API server is unavailable")
		}
		return false, nil, nil
	})
	if rec := do(t, h, "GET", "/containers/web/json", ""); rec.Code != 200 {
		t.Fatalf("inspect = %d %s", rec.Code, rec.Body)
	}
	inspectGets := gets.Load()
	gets.Store(0)
	allowed.Store(inspectGets)

	rec := do(t, h, "GET", "/containers/web/stats?stream=false", "")
	if rec.Code == 200 && rec.Body.Len() == 0 {
		t.Fatal("stats answered 200 without a body")
	}
	if rec.Code < 400 {
		t.Errorf("stats = %d %q, want an error status", rec.Code, rec.Body)
	}
}
