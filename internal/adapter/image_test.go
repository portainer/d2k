package adapter

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/portainer/d2k/internal/types"
)

const (
	nginxDigest   = "sha256:a3554c0d0b48177b1413c26e01f695334428020d9df01243fbae3e11cfd17417"
	redisDigest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	postgreDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	alpineDigest  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func nodeWithImages(name string, images ...corev1.ContainerImage) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Images: images}}
}

func usedPod(namespace, name, app, image, imageID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": app, types.LabelManagedBy: types.LabelManagedByValue}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: app, Image: image, ImageID: imageID,
		}}},
	}
}

func stoppedContainer(name, image string) *appsv1.Deployment {
	d := deployment(name, "eeeeeeee-0000-0000-0000-"+name, managed())
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: name, Image: image}}
	return d
}

func TestListUsedImages(t *testing.T) {
	nginx := corev1.ContainerImage{Names: []string{"docker.io/library/nginx@" + nginxDigest, "docker.io/library/nginx:alpine"}, SizeBytes: 26354394}
	redis := corev1.ContainerImage{Names: []string{"docker.io/library/redis@" + redisDigest, "docker.io/library/redis:7"}, SizeBytes: 40000000}
	postgres := corev1.ContainerImage{Names: []string{"docker.io/library/postgres@" + postgreDigest, "docker.io/library/postgres:16"}, SizeBytes: 150000000}
	portainer := corev1.ContainerImage{Names: []string{"docker.io/portainer/portainer-ee:latest"}, SizeBytes: 220000000}

	a, _ := newTestAdapter(t, []runtime.Object{
		nodeWithImages("node-1", nginx, redis, postgres, portainer),
		// the same image on another node is one image
		nodeWithImages("node-2", nginx),
		// two containers run nginx; its pods name the exact image
		usedPod(testNamespace, "web-1", "web", "nginx:alpine", "docker.io/library/nginx@"+nginxDigest),
		usedPod(testNamespace, "proxy-1", "proxy", "docker.io/library/nginx:alpine", "docker-pullable://docker.io/library/nginx@"+nginxDigest),
		// a stopped container is known by its spec
		stoppedContainer("cache", "redis:7"),
		// another namespace's container, such as another d2k install's
		usedPod("d2k-other", "db-1", "db", "postgres:16", "docker.io/library/postgres@"+postgreDigest),
		// a container whose image is still being pulled
		usedPod(testNamespace, "new-1", "new", "busybox:1.36", ""),
		// a running container whose image no node reports: the kubelet
		// reports only the largest images
		usedPod(testNamespace, "small-1", "small", "alpine:3.20", "docker.io/library/alpine@"+alpineDigest),
	}...)

	images, err := a.ListUsedImages(context.Background())
	if err != nil {
		t.Fatalf("ListUsedImages() error: %v", err)
	}

	byID := map[string]int{}
	for i, img := range images {
		byID[img.ID] = i
	}
	if len(images) != 3 {
		t.Fatalf("ListUsedImages() = %+v, want nginx, redis and alpine only", images)
	}

	got := images[byID[nginxDigest]]
	if got.Size != 26354394 || got.Containers != 2 {
		t.Errorf("nginx = size %d, containers %d; want 26354394, 2", got.Size, got.Containers)
	}
	if len(got.RepoTags) != 1 || got.RepoTags[0] != "nginx:alpine" {
		t.Errorf("nginx tags = %v, want [nginx:alpine]", got.RepoTags)
	}
	if len(got.RepoDigests) != 1 || got.RepoDigests[0] != "nginx@"+nginxDigest {
		t.Errorf("nginx digests = %v, want [nginx@%s]", got.RepoDigests, nginxDigest)
	}

	if _, ok := byID[redisDigest]; !ok {
		t.Errorf("the stopped container's image is missing: %+v", images)
	}

	alpine, ok := byID[alpineDigest]
	if !ok {
		t.Fatalf("the running container's image no node reports is missing: %+v", images)
	}
	if got := images[alpine]; got.Size != 0 || len(got.RepoTags) != 1 || got.RepoTags[0] != "alpine:3.20" || got.RepoDigests[0] != "alpine@"+alpineDigest {
		t.Errorf("alpine = %+v, want alpine:3.20 and its digest, without a size", got)
	}

	for _, id := range []string{nginxDigest, strings.TrimPrefix(nginxDigest, "sha256:")} {
		found, err := a.FindUsedImage(context.Background(), id)
		if err != nil || found == nil || found.ID != nginxDigest {
			t.Errorf("FindUsedImage(%q) = %+v, %v; want nginx", id, found, err)
		}
	}
	if found, _ := a.FindUsedImage(context.Background(), postgreDigest); found != nil {
		t.Errorf("FindUsedImage(postgres) = %+v, want none: another namespace's image", found)
	}
}

// A running container names the image docker images lists, so clients such as
// Portainer show the image as used.
func TestContainerNamesItsImage(t *testing.T) {
	nginx := corev1.ContainerImage{Names: []string{"docker.io/library/nginx@" + nginxDigest, "docker.io/library/nginx:alpine"}, SizeBytes: 26354394}
	a, _ := newTestAdapter(t, []runtime.Object{
		nodeWithImages("node-1", nginx),
		stoppedContainer("web", "nginx:alpine"),
		usedPod(testNamespace, "web-1", "web", "nginx:alpine", "docker.io/library/nginx@"+nginxDigest),
		stoppedContainer("idle", "nginx:alpine"),
	}...)

	containers, err := a.ListContainers(context.Background(), true)
	if err != nil {
		t.Fatalf("ListContainers() error: %v", err)
	}
	ids := map[string]string{}
	for _, c := range containers {
		ids[c.Names[0]] = c.ImageID
	}
	if ids["/web"] != nginxDigest {
		t.Errorf("running container's image ID = %q, want %s", ids["/web"], nginxDigest)
	}
	if ids["/idle"] != "" {
		t.Errorf("stopped container's image ID = %q, want none before a pod runs it", ids["/idle"])
	}

	images, err := a.ListUsedImages(context.Background())
	if err != nil || len(images) != 1 || images[0].ID != ids["/web"] {
		t.Errorf("ListUsedImages() = %+v, %v; want nginx, by the container's image ID", images, err)
	}
}
