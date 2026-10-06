package adapter

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/rs/zerolog/log"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/portainer/d2k/internal/errdefs"
	"github.com/portainer/d2k/internal/types"
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
	state := &eventState{
		known:        map[string]*appsv1.Deployment{},
		restarts:     map[string]int32{},
		terminations: map[string]int32{},
		volumes:      map[string]bool{},
	}
	for ctx.Err() == nil {
		if err := a.watchEvents(ctx, state); err != nil && ctx.Err() == nil {
			log.Warn().Str("context", "KubernetesAdapter").Err(err).Msg("Event watch ended; restarting")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// eventState is the namespace as the events last reported it. It outlives
// each watch, so that what changes while a watch restarts still becomes
// events.
type eventState struct {
	listed       bool
	known        map[string]*appsv1.Deployment
	restarts     map[string]int32
	terminations map[string]int32
	volumes      map[string]bool
}

// watchEvents lists the current state, reporting what changed since the
// last watch ended, then watches Deployments, Pods and PVCs from there.
func (a *KubernetesDockerAdapter) watchEvents(ctx context.Context, state *eventState) error {
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
	first := !state.listed
	for _, m := range state.relist(deps.Items, pods.Items, pvcs.Items, time.Now()) {
		a.events.publish(m)
		if m.Type == events.ContainerEventType && (m.Action == events.ActionDie || m.Action == events.ActionStop) && autoRemoves(state.known, m.Actor.ID) {
			go a.autoRemove(ctx, m.Actor.ID)
		}
	}
	if first {
		// As the Docker daemon does when it starts, remove the AutoRemove
		// containers whose run ended while nothing watched.
		podsByContainer := map[string][]corev1.Pod{}
		for _, p := range pods.Items {
			podsByContainer[p.Labels["app"]] = append(podsByContainer[p.Labels["app"]], p)
		}
		for _, d := range deps.Items {
			if d.Annotations[types.AnnotationAutoRemove] == "true" && containerState(d, podsByContainer[d.Name], time.Now()).Status == container.StateExited {
				go a.autoRemove(ctx, containerID(d.UID))
			}
		}
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
				msgs = deploymentEvents(state.known, e.Type, d, time.Now())
			}
		case e, ok := <-podWatch.ResultChan():
			if !ok {
				return fmt.Errorf("pod watch closed")
			}
			if p, isPod := e.Object.(*corev1.Pod); isPod {
				msgs = podEvents(state.known, state.restarts, state.terminations, e.Type, p, time.Now())
			}
		case e, ok := <-pvcWatch.ResultChan():
			if !ok {
				return fmt.Errorf("volume watch closed")
			}
			if pvc, isPVC := e.Object.(*corev1.PersistentVolumeClaim); isPVC {
				msgs = volumeEvents(state.volumes, e.Type, pvc, time.Now())
			}
		}
		for _, m := range msgs {
			a.events.publish(m)
			if m.Type == events.ContainerEventType && (m.Action == events.ActionDie || m.Action == events.ActionStop) && autoRemoves(state.known, m.Actor.ID) {
				go a.autoRemove(ctx, m.Actor.ID)
			}
		}
	}
}

// autoRemoves reports whether the container is removed when its run ends.
func autoRemoves(known map[string]*appsv1.Deployment, id string) bool {
	for _, d := range known {
		if containerID(d.UID) == id {
			return d.Annotations[types.AnnotationAutoRemove] == "true"
		}
	}
	return false
}

// autoRemove removes a container whose run ended, as Docker does for one
// created with AutoRemove: unless a restart ended the run, or the container
// runs again.
func (a *KubernetesDockerAdapter) autoRemove(ctx context.Context, id string) {
	if resolved, err := a.resolveDeploymentName(ctx, id); err == nil {
		if _, restarting := a.restarting.Load(resolved); restarting {
			return
		}
	}
	if ctr, err := a.InspectContainer(ctx, id); err == nil && (ctr.State.Running || IsStarting(ctr.State)) {
		return
	}
	if err := a.RemoveContainer(ctx, id); err != nil && !errdefs.IsNotFound(err) {
		log.Warn().Str("context", "KubernetesAdapter").Str("id", id).Err(err).Msg("Unable to remove a container whose run ended")
	}
}

// relist records the listed state and returns the events for what changed
// since the last watch, in Docker's order: create, die, start or stop,
// destroy. The first list has none.
func (s *eventState) relist(deps []appsv1.Deployment, pods []corev1.Pod, pvcs []corev1.PersistentVolumeClaim, now time.Time) []events.Message {
	var msgs, changes []events.Message
	gone := maps.Clone(s.known)
	for i := range deps {
		kind := watch.Added
		if _, ok := s.known[deps[i].Name]; ok {
			kind = watch.Modified
		}
		delete(gone, deps[i].Name)
		for _, m := range deploymentEvents(s.known, kind, &deps[i], now) {
			if m.Action == events.ActionCreate {
				msgs = append(msgs, m)
			} else {
				changes = append(changes, m)
			}
		}
	}
	listedPods := map[string]bool{}
	for i := range pods {
		listedPods[pods[i].Name] = true
		msgs = append(msgs, podEvents(s.known, s.restarts, s.terminations, watch.Modified, &pods[i], now)...)
	}
	for name := range s.restarts {
		if !listedPods[name] {
			delete(s.restarts, name)
			delete(s.terminations, name)
		}
	}
	msgs = append(msgs, changes...)
	for _, d := range gone {
		msgs = append(msgs, deploymentEvents(s.known, watch.Deleted, d, now)...)
	}
	goneVolumes := maps.Clone(s.volumes)
	for i := range pvcs {
		delete(goneVolumes, pvcs[i].Name)
		msgs = append(msgs, volumeEvents(s.volumes, watch.Added, &pvcs[i], now)...)
	}
	for name := range goneVolumes {
		msgs = append(msgs, volumeEvents(s.volumes, watch.Deleted, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}, now)...)
	}
	if !s.listed {
		s.listed = true
		return nil
	}
	return msgs
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
		started, stopped := replicasOf(prev) == 0 && replicasOf(d) > 0, replicasOf(prev) > 0 && replicasOf(d) == 0
		if runsAsPod(*d) {
			// The container's pod runs it: its start and stop are recorded.
			started = d.Annotations[types.AnnotationStartedAt] != prev.Annotations[types.AnnotationStartedAt]
			stopped = d.Annotations[types.AnnotationStoppedAt] != prev.Annotations[types.AnnotationStoppedAt]
		}
		if started {
			msgs = append(msgs, containerEvent(d, events.ActionStart, now, nil))
		} else if stopped {
			msgs = append(msgs, containerEvent(d, events.ActionStop, now, nil))
		}
		if oldName := dockerName(*prev); oldName != dockerName(*d) {
			msgs = append(msgs, containerEvent(d, events.ActionRename, now, map[string]string{"oldName": "/" + oldName}))
		}
		return msgs
	}
	return nil
}

// podEvents reports a container's process dying. A Deployment's pod shows
// it as its restart count going up; a container's own pod (restart policy
// "no" or "on-failure") as its container terminating, which terminations
// counts.
func podEvents(known map[string]*appsv1.Deployment, restarts, terminations map[string]int32, kind watch.EventType, p *corev1.Pod, now time.Time) []events.Message {
	if kind == watch.Deleted {
		delete(restarts, p.Name)
		delete(terminations, p.Name)
		return nil
	}
	count, prev := restartCount(p), restarts[p.Name]
	restarts[p.Name] = count
	if p.Spec.RestartPolicy != "" && p.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		count, prev = terminationCount(p), terminations[p.Name]
		terminations[p.Name] = count
	}
	d, ok := known[p.Labels["app"]]
	if !ok || count <= prev {
		return nil
	}
	extra := map[string]string{}
	if s := p.Status.ContainerStatuses; len(s) > 0 {
		if t := lastTermination(&s[0]); t != nil {
			extra["exitCode"] = strconv.Itoa(int(t.ExitCode))
		}
	}
	return []events.Message{containerEvent(d, events.ActionDie, now, extra)}
}

// terminationCount is how many times the pod's container has terminated.
func terminationCount(p *corev1.Pod) int32 {
	s := p.Status.ContainerStatuses
	if len(s) == 0 {
		return 0
	}
	if s[0].State.Terminated != nil {
		return s[0].RestartCount + 1
	}
	return s[0].RestartCount
}

func volumeEvents(volumes map[string]bool, kind watch.EventType, pvc *corev1.PersistentVolumeClaim, now time.Time) []events.Message {
	action := events.ActionCreate
	switch kind {
	case watch.Added:
		if volumes[pvc.Name] {
			return nil
		}
		volumes[pvc.Name] = true
	case watch.Deleted:
		delete(volumes, pvc.Name)
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
