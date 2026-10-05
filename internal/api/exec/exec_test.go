package exec

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
