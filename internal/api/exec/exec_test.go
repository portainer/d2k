package exec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/portainer/d2k/internal/adapter"
	"github.com/portainer/d2k/internal/streams"
)

func TestCreateRejectsWhatKubernetesExecCannotDo(t *testing.T) {
	h := NewHandler(nil) // rejected before the container is looked up
	for _, body := range []string{
		`{"Cmd":["sh"],"User":"root"}`,
		`{"Cmd":["sh"],"WorkingDir":"/tmp"}`,
		`{"Cmd":["sh"],"Env":["A=1"]}`,
		`{"Cmd":["sh"],"Privileged":true}`,
		`{"Cmd":[]}`,
	} {
		rec := httptest.NewRecorder()
		h.Create(rec, httptest.NewRequest("POST", "/containers/web/exec", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("create %s = %d, want 400", body, rec.Code)
		}
	}
}

func TestUnknownExecIsNotFound(t *testing.T) {
	h := NewHandler(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec/{id}/start", h.Start)
	mux.HandleFunc("POST /exec/{id}/resize", h.Resize)
	mux.HandleFunc("GET /exec/{id}/json", h.Inspect)
	for _, req := range []*http.Request{
		httptest.NewRequest("POST", "/exec/nope/start", strings.NewReader("{}")),
		httptest.NewRequest("POST", "/exec/nope/resize?h=1&w=1", nil),
		httptest.NewRequest("GET", "/exec/nope/json", nil),
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
}

// Docker refuses to start an exec whose container stopped or was removed
// after the exec was created, before it takes over the connection: 409 and
// 404, which `docker exec` reports as an error from the daemon.
func TestStartRefusesAContainerThatIsNotRunning(t *testing.T) {
	a := adapter.NewWithClient(fake.NewClientset(), "d2k")
	ctx := context.Background()
	if _, _, err := a.CreateContainer(ctx, adapter.RunOptions{Name: "web", Image: "nginx"}); err != nil {
		t.Fatal(err)
	}
	stopped, err := a.InspectContainer(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(a)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /exec/{id}/start", h.Start)
	for _, tc := range []struct {
		containerID, body string
		want              int
	}{
		{stopped.ID, `{}`, http.StatusConflict},
		{stopped.ID, `{"Detach":true}`, http.StatusConflict},
		{"removed", `{}`, http.StatusNotFound},
	} {
		h.instances["e"] = &instance{id: "e", containerID: tc.containerID, opts: adapter.ExecOptions{Name: tc.containerID, Cmd: []string{"true"}}, resize: streams.NewSizeQueue()}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/exec/e/start", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("start on %s with %s = %d %q, want %d", tc.containerID, tc.body, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// As the Docker daemon does (moby daemon/errors.go setExitCodeFromError,
// which its exec start uses), an exec whose command does not exist exits
// 127 and one whose command can't be invoked exits 126. The CRI reports
// both as the runtime's error text.
func TestStartErrorExitCode(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want int
	}{
		{`OCI runtime exec failed: exec failed: unable to start container process: exec: "nosuch": executable file not found in $PATH: unknown`, 127},
		{`OCI runtime exec failed: exec failed: unable to start container process: exec: "/no/such": stat /no/such: no such file or directory: unknown`, 127},
		{`OCI runtime exec failed: exec failed: unable to start container process: exec: "/etc/hosts": permission denied: unknown`, 126},
		{`OCI runtime exec failed: exec failed: unable to start container process: exec: "/etc": is a directory: unknown`, 126},
	} {
		if got := startErrorExitCode(errors.New(tc.err)); got != tc.want {
			t.Errorf("exit code for %q = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// An exec instance that is created but never started, as a client that fails
// between the two calls leaves, is forgotten too, as a finished one is.
func TestUnstartedExecIsForgotten(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-finishedRetention)

	old := &instance{created: now.Add(-2 * finishedRetention)}
	if !old.isStaleBefore(cutoff) {
		t.Error("an instance never started for longer than the retention is kept")
	}
	recent := &instance{created: now}
	if recent.isStaleBefore(cutoff) {
		t.Error("an instance just created is forgotten before it can start")
	}
	running := &instance{created: now.Add(-2 * finishedRetention)}
	running.markStarted()
	if running.isStaleBefore(cutoff) {
		t.Error("a running instance is forgotten")
	}
	running.finish(0)
	running.finished = now.Add(-2 * finishedRetention)
	if !running.isStaleBefore(cutoff) {
		t.Error("an instance finished for longer than the retention is kept")
	}
}
