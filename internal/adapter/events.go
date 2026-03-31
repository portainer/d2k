package adapter

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// DockerEvent mirrors the Docker event wire format.
type DockerEvent struct {
	Type   string            `json:"Type"`
	Action string            `json:"Action"`
	Actor  DockerEventActor  `json:"Actor"`
	Time   int64             `json:"time"`
	TimeNano int64           `json:"timeNano"`
}

// DockerEventActor holds the event subject.
type DockerEventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// WatchEvents watches the namespace for Kubernetes resource changes and
// converts them to Docker events, sending them to the returned channel.
// The channel is closed when ctx is cancelled.
func (a *KubernetesDockerAdapter) WatchEvents(ctx context.Context) (<-chan DockerEvent, error) {
	out := make(chan DockerEvent, 64)
	// Send historical events first.
	a.sendHistoricalEvents(ctx, out)

	deployWatch, err := a.client.AppsV1().Deployments(a.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: "d2k.portainer.io/managed-by=d2k",
	})
	if err != nil {
		return nil, err
	}

	podWatch, err := a.client.CoreV1().Pods(a.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: "d2k.portainer.io/managed-by=d2k",
	})
	if err != nil {
		deployWatch.Stop()
		return nil, err
	}

	pvcWatch, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: "d2k.portainer.io/managed-by=d2k",
	})
	if err != nil {
		deployWatch.Stop()
		podWatch.Stop()
		return nil, err
	}

	svcWatch, err := a.client.CoreV1().Services(a.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: "d2k.portainer.io/managed-by=d2k",
	})
	if err != nil {
		deployWatch.Stop()
		podWatch.Stop()
		pvcWatch.Stop()
		return nil, err
	}

	go func() {
		defer close(out)
		defer deployWatch.Stop()
		defer podWatch.Stop()
		defer pvcWatch.Stop()
		defer svcWatch.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case e, ok := <-deployWatch.ResultChan():
				if !ok {
					return
				}
				if ev := deploymentEvent(e); ev != nil {
					out <- *ev
				}

			case e, ok := <-podWatch.ResultChan():
				if !ok {
					return
				}
				if ev := podEvent(e); ev != nil {
					out <- *ev
				}

			case e, ok := <-pvcWatch.ResultChan():
				if !ok {
					return
				}
				if ev := pvcEvent(e); ev != nil {
					out <- *ev
				}

			case e, ok := <-svcWatch.ResultChan():
				if !ok {
					return
				}
				if ev := svcEvent(e); ev != nil {
					out <- *ev
				}
			}
		}
	}()

	return out, nil
}

func (a *KubernetesDockerAdapter) sendHistoricalEvents(ctx context.Context, out chan<- DockerEvent) {
	events, err := a.client.CoreV1().Events(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, e := range events.Items {
		action := ""
		eventType := ""

		switch e.InvolvedObject.Kind {
		case "Pod":
			eventType = "container"
			switch e.Reason {
			case "Started":
				action = "start"
			case "Killing":
				action = "stop"
			case "Created":
				action = "create"
			default:
				continue
			}
		case "PersistentVolumeClaim":
			eventType = "volume"
			switch e.Reason {
			case "ProvisioningSucceeded":
				action = "create"
			default:
				continue
			}
		default:
			continue
		}

		t := e.LastTimestamp.Time
		out <- DockerEvent{
			Type:     eventType,
			Action:   action,
			Time:     t.Unix(),
			TimeNano: t.UnixNano(),
			Actor: DockerEventActor{
				ID: string(e.InvolvedObject.UID),
				Attributes: map[string]string{
					"name": e.InvolvedObject.Name,
				},
			},
		}
	}
}

func deploymentEvent(e watch.Event) *DockerEvent {
	d, ok := e.Object.(*appsv1.Deployment)
	if !ok {
		return nil
	}

	action := ""
	switch e.Type {
	case watch.Added:
		action = "create"
	case watch.Deleted:
		action = "destroy"
	default:
		return nil
	}

	now := time.Now()
	return &DockerEvent{
		Type:     "container",
		Action:   action,
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		Actor: DockerEventActor{
			ID: string(d.UID),
			Attributes: map[string]string{
				"name":  d.Name,
				"image": d.Annotations["d2k.portainer.io/image-ref"],
			},
		},
	}
}

func podEvent(e watch.Event) *DockerEvent {
	p, ok := e.Object.(*corev1.Pod)
	if !ok {
		return nil
	}

	action := ""
	switch e.Type {
	case watch.Added:
		action = "start"
	case watch.Deleted:
		action = "stop"
	default:
		return nil
	}

	now := time.Now()
	return &DockerEvent{
		Type:     "container",
		Action:   action,
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		Actor: DockerEventActor{
			ID: string(p.UID),
			Attributes: map[string]string{
				"name":  p.Labels["app"],
				"image": p.Spec.Containers[0].Image,
			},
		},
	}
}

func pvcEvent(e watch.Event) *DockerEvent {
	pvc, ok := e.Object.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}

	action := ""
	switch e.Type {
	case watch.Added:
		action = "create"
	case watch.Deleted:
		action = "destroy"
	default:
		return nil
	}

	now := time.Now()
	return &DockerEvent{
		Type:     "volume",
		Action:   action,
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		Actor: DockerEventActor{
			ID: pvc.Name,
			Attributes: map[string]string{
				"driver": "d2k",
			},
		},
	}
}

func svcEvent(e watch.Event) *DockerEvent {
	svc, ok := e.Object.(*corev1.Service)
	if !ok {
		return nil
	}

	action := ""
	switch e.Type {
	case watch.Added:
		action = "connect"
	case watch.Deleted:
		action = "disconnect"
	default:
		return nil
	}

	now := time.Now()
	return &DockerEvent{
		Type:     "network",
		Action:   action,
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		Actor: DockerEventActor{
			ID: string(svc.UID),
			Attributes: map[string]string{
				"name": svc.Name,
			},
		},
	}
}
