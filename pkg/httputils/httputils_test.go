package httputils

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestParseFilters(t *testing.T) {
	accepted := map[string]bool{"label": true, "name": true}

	tests := []struct {
		name      string
		raw       string
		wantErr   bool
		wantLabel []string
	}{
		{name: "absent", raw: ""},
		{name: "map form", raw: `{"label":{"a=b":true}}`, wantLabel: []string{"a=b"}},
		{name: "legacy array form", raw: `{"label":["a=b"]}`, wantLabel: []string{"a=b"}},
		{name: "unsupported key", raw: `{"ancestor":{"nginx":true}}`, wantErr: true},
		{name: "invalid json", raw: `{label`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/containers/json?filters="+url.QueryEscape(tt.raw), nil)
			args, err := ParseFilters(r, accepted)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseFilters() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFilters() error: %v", err)
			}
			got := args.Get("label")
			if len(got) != len(tt.wantLabel) || (len(got) > 0 && got[0] != tt.wantLabel[0]) {
				t.Fatalf("label filter = %v, want %v", got, tt.wantLabel)
			}
		})
	}
}
