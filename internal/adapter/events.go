package adapter

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/rs/zerolog/log"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// eventHistorySize is how many past events /events can replay with since.
const eventHistorySize = 1024

// eventHub keeps the namespace's recent Docker events, as the daemon does,
// and fans new ones out to subscribers.
type eventHub struct {
	mu          sync.Mutex
	history     []events.Message
	subscribers map[chan events.Message]struct{}
}

// publish records m and sends it to every subscriber. A subscriber that is
// not keeping up misses the event rather than holding up the others.
func (h *eventHub) publish(m events.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history = append(h.history, m)
	if len(h.history) > eventHistorySize {
		h.history = h.history[len(h.history)-eventHistorySize:]
	}
	for sub := range h.subscribers {
		select {
		case sub <- m:
		default:
		}
	}
}

// subscribe returns the recorded events and a channel of new ones, with no
// gap between them. cancel ends the subscription.
func (h *eventHub) subscribe() (history []events.Message, live <-chan events.Message, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan events.Message, 256)
	if h.subscribers == nil {
		h.subscribers = map[chan events.Message]struct{}{}
	}
	h.subscribers[ch] = struct{}{}
	var once sync.Once
	return append([]events.Message(nil), h.history...), ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers, ch)
			h.mu.Unlock()
		})
	}
}

// SubscribeEvents returns the recorded events and a channel of new ones.
// The caller calls cancel when it stops reading.
func (a *KubernetesDockerAdapter) SubscribeEvents() ([]events.Message, <-chan events.Message, func()) {
	return a.events.subscribe()
}

// RecordEvents watches the namespace and records Docker events until ctx
// ends. Watches that end, as Kubernetes ends them periodically, restart.
func (a *KubernetesDockerAdapter) RecordEvents(ctx context.Context) {
	for ctx.Err() == nil {
		if err := a.watchEvents(ctx); err != nil && ctx.Err() == nil {
			log.Warn().Str("context", "KubernetesAdapter").Err(err).Msg("Event watch ended; restarting")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// watchEvents lists the current state, so that only changes become events,
// then watches Deployments, Pods and PVCs from there.
func (a *KubernetesDockerAdapter) watchEvents(ctx context.Context) error {
	deps, err := a.client.AppsV1().Deployments(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return fmt.Errorf("unable to list deployments: %w", err)
	}
	pods, err := a.client.CoreV1().Pods(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return fmt.Errorf("unable to list pods: %w", err)
	}
	pvcs, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).List(ctx, metav1ListOptions())
	if err != nil {
		return fmt.Errorf("unable to list volumes: %w", err)
	}

	known := map[string]*appsv1.Deployment{}
	for i := range deps.Items {
		known[deps.Items[i].Name] = &deps.Items[i]
	}
	restarts := map[string]int32{}
	for i := range pods.Items {
		restarts[pods.Items[i].Name] = restartCount(&pods.Items[i])
	}

	opts := metav1ListOptions()
	opts.ResourceVersion = deps.ResourceVersion
	depWatch, err := a.client.AppsV1().Deployments(a.namespace).Watch(ctx, opts)
	if err != nil {
		return err
	}
	defer depWatch.Stop()
	opts.ResourceVersion = pods.ResourceVersion
	podWatch, err := a.client.CoreV1().Pods(a.namespace).Watch(ctx, opts)
	if err != nil {
		return err
	}
	defer podWatch.Stop()
	opts.ResourceVersion = pvcs.ResourceVersion
	pvcWatch, err := a.client.CoreV1().PersistentVolumeClaims(a.namespace).Watch(ctx, opts)
	if err != nil {
		return err
	}
	defer pvcWatch.Stop()

	for {
		var msgs []events.Message
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-depWatch.ResultChan():
			if !ok {
				return fmt.Errorf("deployment watch closed")
			}
			if d, isDeployment := e.Object.(*appsv1.Deployment); isDeployment {
				msgs = deploymentEvents(known, e.Type, d, time.Now())
			}
		case e, ok := <-podWatch.ResultChan():
			if !ok {
				return fmt.Errorf("pod watch closed")
			}
			if p, isPod := e.Object.(*corev1.Pod); isPod {
				msgs = podEvents(known, restarts, e.Type, p, time.Now())
			}
		case e, ok := <-pvcWatch.ResultChan():
			if !ok {
				return fmt.Errorf("volume watch closed")
			}
			if pvc, isPVC := e.Object.(*corev1.PersistentVolumeClaim); isPVC {
				msgs = volumeEvents(e.Type, pvc, time.Now())
			}
		}
		for _, m := range msgs {
			a.events.publish(m)
		}
	}
}

// deploymentEvents turns a change to a container's Deployment into the
// container events Docker emits, tracking the last state in known.
func deploymentEvents(known map[string]*appsv1.Deployment, kind watch.EventType, d *appsv1.Deployment, now time.Time) []events.Message {
	prev, existed := known[d.Name]
	switch kind {
	case watch.Added:
		known[d.Name] = d
		if existed {
			return nil
		}
		return []events.Message{containerEvent(d, events.ActionCreate, now, nil)}
	case watch.Deleted:
		delete(known, d.Name)
		return []events.Message{containerEvent(d, events.ActionDestroy, now, nil)}
	case watch.Modified:
		known[d.Name] = d
		if !existed {
			return nil
		}
		var msgs []events.Message
		if was, is := replicasOf(prev), replicasOf(d); was == 0 && is > 0 {
			msgs = append(msgs, containerEvent(d, events.ActionStart, now, nil))
		} else if was > 0 && is == 0 {
			msgs = append(msgs, containerEvent(d, events.ActionStop, now, nil))
		}
		if oldName := dockerName(*prev); oldName != dockerName(*d) {
			msgs = append(msgs, containerEvent(d, events.ActionRename, now, map[string]string{"oldName": "/" + oldName}))
		}
		return msgs
	}
	return nil
}

// podEvents reports a container's process dying, which Kubernetes shows as
// its pod's restart count going up.
func podEvents(known map[string]*appsv1.Deployment, restarts map[string]int32, kind watch.EventType, p *corev1.Pod, now time.Time) []events.Message {
	if kind == watch.Deleted {
		delete(restarts, p.Name)
		return nil
	}
	count, prev := restartCount(p), restarts[p.Name]
	restarts[p.Name] = count
	d, ok := known[p.Labels["app"]]
	if !ok || count <= prev {
		return nil
	}
	extra := map[string]string{}
	if s := p.Status.ContainerStatuses; len(s) > 0 && s[0].LastTerminationState.Terminated != nil {
		extra["exitCode"] = strconv.Itoa(int(s[0].LastTerminationState.Terminated.ExitCode))
	}
	return []events.Message{containerEvent(d, events.ActionDie, now, extra)}
}

func volumeEvents(kind watch.EventType, pvc *corev1.PersistentVolumeClaim, now time.Time) []events.Message {
	action := events.ActionCreate
	switch kind {
	case watch.Added:
	case watch.Deleted:
		action = events.ActionDestroy
	default:
		return nil
	}
	return []events.Message{{
		Type:     events.VolumeEventType,
		Action:   action,
		Actor:    events.Actor{ID: pvc.Name, Attributes: map[string]string{"driver": "d2k"}},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
	}}
}

// containerEvent builds a container event whose attributes are the
// container's name, image and labels, as in Docker.
func containerEvent(d *appsv1.Deployment, action events.Action, now time.Time, extra map[string]string) events.Message {
	attrs := maps.Clone(dockerLabels(*d))
	if attrs == nil {
		attrs = map[string]string{}
	}
	attrs["name"] = dockerName(*d)
	if image := d.Annotations["d2k.portainer.io/image-ref"]; image != "" {
		attrs["image"] = image
	}
	maps.Copy(attrs, extra)
	return events.Message{
		Type:     events.ContainerEventType,
		Action:   action,
		Actor:    events.Actor{ID: containerID(d.UID), Attributes: attrs},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
	}
}

func replicasOf(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

func restartCount(p *corev1.Pod) int32 {
	if s := p.Status.ContainerStatuses; len(s) > 0 {
		return s[0].RestartCount
	}
	return 0
}
