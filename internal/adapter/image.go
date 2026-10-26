package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/image"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/portainer/d2k/internal/types"
)

// ListUsedImages returns the images the containers of d2k's namespace use,
// as docker images lists them. d2k stores no images: the kubelet pulls them
// on the nodes, which report the images they hold, with their names and
// sizes. Only the images of d2k's own containers are listed, so that other
// namespaces' images, such as those of other d2k installs, stay theirs.
//
// The kubelet reports only the largest images of each node, 50 by default:
// an image a running container uses that no node reports is still listed,
// from its pod, without a size. A stopped container's image is listed when a
// node reports it, and an image still being pulled isn't listed.
func (a *KubernetesDockerAdapter) ListUsedImages(ctx context.Context) ([]image.Summary, error) {
	nodes, err := a.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to list nodes: %w", err)
	}
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list pods: %w", err)
	}
	deployments, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: types.LabelManagedBy + "=" + types.LabelManagedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list deployments: %w", err)
	}

	byName := nodeImagesByName(nodes.Items)
	used := map[string]*image.Summary{}
	var order []string
	containers := map[string]map[string]bool{}
	use := func(summary image.Summary, container string) {
		if _, ok := used[summary.ID]; !ok {
			used[summary.ID] = &summary
			order = append(order, summary.ID)
			containers[summary.ID] = map[string]bool{}
		}
		containers[summary.ID][container] = true
	}

	// A running container's pod names the exact image it runs; a container
	// without pods, stopped, is known by the image of its spec.
	withPods := map[string]bool{}
	for _, p := range pods.Items {
		container := p.Labels["app"]
		for _, status := range p.Status.ContainerStatuses {
			digest := digestName(status.ImageID)
			if digest == "" {
				continue // still being pulled
			}
			withPods[container] = true
			if nodeImage := byName[digest]; nodeImage != nil {
				use(imageSummary(*nodeImage), container)
				continue
			}
			use(podImageSummary(status.Image, digest), container)
		}
	}
	for _, d := range deployments.Items {
		if withPods[d.Name] {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if nodeImage := byName[normalizedName(c.Image)]; nodeImage != nil {
				use(imageSummary(*nodeImage), d.Name)
			}
		}
	}

	summaries := make([]image.Summary, 0, len(order))
	for _, id := range order {
		summary := *used[id]
		summary.Containers = int64(len(containers[id]))
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// FindUsedImage returns the image, among those the containers of d2k's
// namespace use, whose ID is id, with or without its sha256: prefix. It
// returns nil when no used image has that ID.
func (a *KubernetesDockerAdapter) FindUsedImage(ctx context.Context, id string) (*image.Summary, error) {
	images, err := a.ListUsedImages(ctx)
	if err != nil {
		return nil, err
	}

	for _, img := range images {
		if img.ID == id || strings.TrimPrefix(img.ID, "sha256:") == id {
			return &img, nil
		}
	}
	return nil, nil
}

// nodeImagesByName indexes the images the nodes report by each of their
// names, in full: docker.io/library/nginx:alpine and
// docker.io/library/nginx@sha256:….
func nodeImagesByName(nodes []corev1.Node) map[string]*corev1.ContainerImage {
	byName := map[string]*corev1.ContainerImage{}
	for i := range nodes {
		for j := range nodes[i].Status.Images {
			nodeImage := &nodes[i].Status.Images[j]
			for _, name := range nodeImage.Names {
				if _, ok := byName[name]; !ok {
					byName[name] = nodeImage
				}
			}
		}
	}
	return byName
}

// imageSummary describes a node's image as docker images does. The ID is
// the image's digest: the kubelet reports no Docker image ID. d2k doesn't
// know when the image was built, so Created is the zero time.
func imageSummary(nodeImage corev1.ContainerImage) image.Summary {
	summary := image.Summary{
		RepoTags:    []string{},
		RepoDigests: []string{},
		Labels:      map[string]string{},
		Size:        nodeImage.SizeBytes,
		SharedSize:  -1,
	}
	for _, name := range nodeImage.Names {
		named, err := reference.ParseNormalizedNamed(name)
		if err != nil {
			continue
		}
		if canonical, ok := named.(reference.Canonical); ok {
			summary.RepoDigests = append(summary.RepoDigests, reference.FamiliarString(named))
			if summary.ID == "" {
				summary.ID = canonical.Digest().String()
			}
			continue
		}
		summary.RepoTags = append(summary.RepoTags, reference.FamiliarString(named))
	}
	if summary.ID == "" {
		sum := sha256.Sum256([]byte(strings.Join(nodeImage.Names, ",")))
		summary.ID = "sha256:" + hex.EncodeToString(sum[:])
	}
	return summary
}

// podImageSummary describes the image a pod reports running, when no node
// reports it: by its reference and digest, without a size.
func podImageSummary(ref, digest string) image.Summary {
	summary := image.Summary{RepoTags: []string{}, RepoDigests: []string{}, Labels: map[string]string{}, SharedSize: -1}
	if named, err := reference.ParseNormalizedNamed(digest); err == nil {
		summary.RepoDigests = append(summary.RepoDigests, reference.FamiliarString(named))
		if canonical, ok := named.(reference.Canonical); ok {
			summary.ID = canonical.Digest().String()
		}
	}
	if named, err := reference.ParseNormalizedNamed(ref); err == nil {
		if _, digested := named.(reference.Canonical); !digested {
			summary.RepoTags = append(summary.RepoTags, reference.FamiliarString(reference.TagNameOnly(named)))
		}
	}
	return summary
}

// digestName returns the full name@digest of a pod's image ID, as nodes
// name their images, or "" when it has none. The runtime may prefix the ID,
// as docker-pullable://.
func digestName(imageID string) string {
	if i := strings.Index(imageID, "://"); i >= 0 {
		imageID = imageID[i+3:]
	}
	if !strings.Contains(imageID, "@") {
		return ""
	}
	return normalizedName(imageID)
}

// normalizedName returns the full name of an image reference, as nodes name
// their images: nginx:alpine is docker.io/library/nginx:alpine, and nginx is
// docker.io/library/nginx:latest. It returns "" for an invalid reference.
func normalizedName(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	return reference.TagNameOnly(named).String()
}
