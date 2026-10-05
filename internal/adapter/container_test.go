package adapter

import (
	"context"
	"testing"
)

func TestResolveDeploymentName(t *testing.T) {
	a, _ := newTestAdapter(t,
		deployment("web", "2665dc61-cb7a-4c1e-9f00-000000000001", managed()),
		deployment("my-web", "7f3a9b20-1111-4c1e-9f00-000000000002", managed()),
		deployment("my-api", "5b0c1d2e-3333-4c1e-9f00-000000000004", managed()),
		// d2k's own Deployment carries no managed-by label.
		deployment("d2k", "aaaaaaaa-2222-4c1e-9f00-000000000003", map[string]string{"app": "d2k"}),
	)

	tests := []struct {
		nameOrID string
		want     string
		wantErr  bool
	}{
		{nameOrID: "web", want: "web"},
		{nameOrID: "my-web", want: "my-web"},
		{nameOrID: "2665dc61-cb7a-4c1e-9f00-000000000001", want: "web"},
		{nameOrID: "2665dc61-cb7", want: "web"},
		{nameOrID: "7f3a9b20", want: "my-web"},
		// Docker never resolves a name by suffix: "api" must not match "my-api".
		{nameOrID: "api", wantErr: true},
		// Unmanaged Deployments, including d2k itself, are invisible.
		{nameOrID: "d2k", wantErr: true},
		{nameOrID: "aaaaaaaa-2222-4c1e-9f00-000000000003", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.nameOrID, func(t *testing.T) {
			got, err := a.resolveDeploymentName(context.Background(), tt.nameOrID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveDeploymentName(%q) = %q, want error", tt.nameOrID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDeploymentName(%q) error: %v", tt.nameOrID, err)
			}
			if got != tt.want {
				t.Fatalf("resolveDeploymentName(%q) = %q, want %q", tt.nameOrID, got, tt.want)
			}
		})
	}
}
