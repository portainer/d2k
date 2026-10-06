package exec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
