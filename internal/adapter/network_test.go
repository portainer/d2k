package adapter

import (
	"context"
	"maps"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/errdefs"
)

// Networks are recorded on the namespace, so they outlive d2k: a restarted
// d2k reports them exactly as they were created, joined or not.
func TestNetworksSurviveRestart(t *testing.T) {
	a, client := newTestAdapter(t)
	ctx := context.Background()
	labels := map[string]string{
		"com.docker.compose.project": "netloss", "com.docker.compose.network": "front_end",
		"com.docker.compose.config-hash": strings.Repeat("f", 64), "team": "Payments Team",
	}
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "netloss_front_end", Labels: labels, Internal: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateNetwork(ctx, CreateNetworkOptions{Name: "netloss_front_end"}); !errdefs.IsConflict(err) {
		t.Fatalf("second create = %v, want conflict", err)
	}

	restarted := NewWithClient(client, testNamespace)
	n, err := restarted.InspectNetwork(ctx, "netloss_front_end")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range labels {
		if n.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, n.Labels[k], v)
		}
	}
	byID, err := restarted.InspectNetwork(ctx, n.ID)
	if err != nil || byID.Name != "netloss_front_end" {
		t.Errorf("inspect by ID = %+v, %v", byID, err)
	}
	if w := restarted.networkWarnings(ctx, []string{"netloss_front_end"}); len(w) != 1 || !strings.Contains(w[0], "internal") {
		t.Errorf("warnings after restart = %q", w)
	}
	list, _ := restarted.ListNetworks(ctx)
	if !containsNetwork(list, "netloss_front_end") {
		t.Errorf("list after restart misses the network")
	}

	// The record is an annotation on the namespace, and only that.
	ns, _ := client.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
	keys := slicesOf(maps.Keys(ns.Annotations))
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "d2k.portainer.io/network-") || len(ns.Labels) != 0 {
		t.Errorf("namespace annotations = %q, labels = %v", keys, ns.Labels)
	}

	if err := restarted.RemoveNetwork(ctx, "netloss_front_end"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InspectNetwork(ctx, "netloss_front_end"); !errdefs.IsNotFound(err) {
		t.Errorf("inspect after remove = %v, want not found", err)
	}
}

func containsNetwork(list []NetworkSummary, name string) bool {
	for _, n := range list {
		if n.Name == name {
			return true
		}
	}
	return false
}

func slicesOf(seq func(func(string) bool)) []string {
	var out []string
	for k := range seq {
		out = append(out, k)
	}
	return out
}
