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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		terminations: map[string]int32{},
		podApp:       map[string]string{},
		running:      map[string]string{},
		died:         map[string]death{},
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
	terminations map[string]int32
	podApp       map[string]string
	running      map[string]string // app of each pod whose container runs
	died         map[string]death  // last death of each container, by app, until it runs again
	volumes      map[string]bool
}

// death is a container's death: the pod it died in, and when.
type death struct {
	pod string
	at  time.Time
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
		a.finishRestarts(ctx, deps.Items)
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
				now := time.Now()
				if e.Type == watch.Deleted {
					msgs = state.removedEvents(d.Name, now)
				}
				msgs = append(msgs, deploymentEvents(state.known, e.Type, d, now)...)
			}
		case e, ok := <-podWatch.ResultChan():
			if !ok {
				return fmt.Errorf("pod watch closed")
			}
			if p, isPod := e.Object.(*corev1.Pod); isPod {
				now := time.Now()
				msgs = podEvents(state.known, state.terminations, state.podApp, e.Type, p, now)
				msgs = append(msgs, state.restartEvents(p.Labels["app"], p.Name, e.Type != watch.Deleted && containerRuns(p), len(msgs) > 0, now)...)
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

// finishRestarts finishes the restarts d2k stopping cut short, as the Docker
// daemon starts its always and unless-stopped containers when it starts. A
// container that runs once stays as the restart left it, as in Docker.
func (a *KubernetesDockerAdapter) finishRestarts(ctx context.Context, deps []appsv1.Deployment) {
	for _, d := range deps {
		if _, restarting := d.Annotations[types.AnnotationRestarting]; !restarting {
			continue
		}
		if !runsAsPod(d) && d.Labels[types.LabelSwarmManagedBy] != types.LabelSwarmManagedByValue {
			if err := a.scaleDeployment(ctx, d.Name, 1); err != nil {
				log.Warn().Str("context", "KubernetesAdapter").Str("container", d.Name).Err(err).Msg("Unable to finish a restart")
				continue
			}
		}
		if err := a.annotate(ctx, d.Name, map[string]*string{types.AnnotationRestarting: nil}); err != nil && !apierrors.IsNotFound(err) {
			log.Warn().Str("context", "KubernetesAdapter").Str("container", d.Name).Err(err).Msg("Unable to clear the restarting mark")
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

// attachDrain is how long autoRemove waits for a container's attaches to
// end. An attach ends at most 10 seconds after its container's die event.
const attachDrain = 15 * time.Second

// Attaching records an attach to the container id until done is called.
func (a *KubernetesDockerAdapter) Attaching(id string) (done func()) {
	a.attachesMu.Lock()
	defer a.attachesMu.Unlock()
	if a.attaches == nil {
		a.attaches = map[string]int{}
	}
	a.attaches[id]++
	return func() {
		a.attachesMu.Lock()
		defer a.attachesMu.Unlock()
		if a.attaches[id]--; a.attaches[id] == 0 {
			delete(a.attaches, id)
		}
	}
}

// attached reports whether the container id has an attach in progress.
func (a *KubernetesDockerAdapter) attached(id string) bool {
	a.attachesMu.Lock()
	defer a.attachesMu.Unlock()
	return a.attaches[id] > 0
}

// autoRemove removes a container whose run ended, as Docker does for one
// created with AutoRemove: unless a restart ended the run, or the container
// runs again.
func (a *KubernetesDockerAdapter) autoRemove(ctx context.Context, id string) {
	// As Docker does, remove it only once its attaches have sent the run's
	// output (D2K-051): docker run attaches before it starts the container.
	for deadline := time.Now().Add(attachDrain); a.attached(id) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	// A container being restarted inspects as running.
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
		died := podEvents(s.known, s.terminations, s.podApp, watch.Modified, &pods[i], now)
		msgs = append(msgs, died...)
		msgs = append(msgs, s.restartEvents(pods[i].Labels["app"], pods[i].Name, containerRuns(&pods[i]), len(died) > 0, now)...)
	}
	for name, app := range s.podApp {
		if listedPods[name] {
			continue
		}
		// The pod disappeared while nothing watched: a relist across a watch
		// restart (D2K-021) sees it gone rather than a Deleted event. If its
		// container was last seen running, its end was never reported.
		lost := podLostEvent(s.known, s.terminations, app, name, now)
		if d, ok := s.known[app]; ok && len(lost) == 0 && s.running[name] == app {
			lost = []events.Message{containerEvent(d, events.ActionDie, now, map[string]string{"exitCode": "137"})}
		}
		msgs = append(msgs, lost...)
		msgs = append(msgs, s.restartEvents(app, name, false, len(lost) > 0, now)...)
		delete(s.terminations, name)
		delete(s.podApp, name)
	}
	msgs = append(msgs, changes...)
	for _, d := range gone {
		msgs = append(msgs, s.removedEvents(d.Name, now)...)
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

// podEvents reports a container's process dying: its pod's container
// terminating, which terminations counts. That includes a Deployment's pod
// ended by docker stop, which the kubelet shows terminated before the pod
// goes (D2K-058). podApp remembers each pod's app label, for when it is
// finally deleted and the event no longer carries one.
func podEvents(known map[string]*appsv1.Deployment, terminations map[string]int32, podApp map[string]string, kind watch.EventType, p *corev1.Pod, now time.Time) []events.Message {
	if kind == watch.Deleted {
		msgs := podLostEvent(known, terminations, p.Labels["app"], p.Name, now)
		delete(terminations, p.Name)
		delete(podApp, p.Name)
		return msgs
	}
	podApp[p.Name] = p.Labels["app"]
	// In back-off the kubelet shows the last crash as Waiting, with the
	// restart count it had: the count drops by one until the restart.
	count, prev := terminationCount(p), terminations[p.Name]
	terminations[p.Name] = max(count, prev)
	d, ok := known[p.Labels["app"]]
	if !ok {
		return nil
	}
	if count <= prev {
		return podFailedWithoutTermination(d, terminations, p, now)
	}
	extra := map[string]string{}
	if s := p.Status.ContainerStatuses; len(s) > 0 {
		if t := lastTermination(&s[0]); t != nil {
			extra["exitCode"] = strconv.Itoa(int(t.ExitCode))
		}
	}
	return []events.Message{containerEvent(d, events.ActionDie, now, extra)}
}

// podFailedWithoutTermination reports a run-once pod that ended Failed
// without the kubelet ever recording a container termination: rejected by
// admission (e.g. OutOfcpu) before it got to run. Without this, no die is
// sent until the pod is later deleted (docker start replacing it to retry),
// arriving stale during that next run.
func podFailedWithoutTermination(d *appsv1.Deployment, terminations map[string]int32, p *corev1.Pod, now time.Time) []events.Message {
	if !runsAsPod(*d) || p.Status.Phase != corev1.PodFailed || len(p.Status.ContainerStatuses) > 0 || terminations[p.Name] != 0 {
		return nil
	}
	// Mark it reported so podLostEvent does not send a second, stale die
	// when this pod is later deleted.
	terminations[p.Name] = 1
	return []events.Message{containerEvent(d, events.ActionDie, now, map[string]string{"exitCode": "137"})}
}

// restartEvents reports a container running again after a death that d2k's
// own docker start did not follow: the kubelet restarting it in its pod, or
// a new pod replacing its pod (deleted by hand, drained, evicted). As in
// Docker, each restart is a start, on which attached compose up follows the
// new run (D2K-070). A container that runs as a pod restarts only in the
// pod it died in; its next pod is a docker start.
func (s *eventState) restartEvents(app, pod string, runs, died bool, now time.Time) []events.Message {
	if runs {
		s.running[pod] = app
	} else {
		delete(s.running, pod)
	}
	d, ok := s.known[app]
	if !ok {
		delete(s.died, app)
		return nil
	}
	if died {
		s.died[app] = death{pod: pod, at: now}
	}
	last, ok := s.died[app]
	if !ok {
		return nil
	}
	if started, err := time.Parse(time.RFC3339Nano, d.Annotations[types.AnnotationStartedAt]); err == nil && started.After(last.at) {
		// docker start ran it again, and reported its start.
		delete(s.died, app)
		return nil
	}
	for p, a := range s.running {
		if a == app && (!runsAsPod(*d) || p == last.pod) {
			delete(s.died, app)
			return []events.Message{containerEvent(d, events.ActionStart, now, nil)}
		}
	}
	return nil
}

// removedEvents reports the death of a container removed while it runs,
// as docker rm -f kills it, with 137, before it removes it (D2K-072): its
// Deployment is gone before its pod ends, and the pod's end is then no
// longer reported.
func (s *eventState) removedEvents(app string, now time.Time) []events.Message {
	d, ok := s.known[app]
	delete(s.died, app)
	runs := false
	for p, a := range s.running {
		if a == app {
			runs = true
			delete(s.running, p)
		}
	}
	if !ok || !runs {
		return nil
	}
	return []events.Message{containerEvent(d, events.ActionDie, now, map[string]string{"exitCode": "137"})}
}

// containerRuns reports whether the pod's container is running, in a pod
// that is not being deleted.
func containerRuns(p *corev1.Pod) bool {
	s := p.Status.ContainerStatuses
	return p.DeletionTimestamp == nil && len(s) > 0 && s[0].State.Running != nil
}

// podLostEvent reports a run-once container's pod ending without ever
// reporting a termination and without d2k having recorded its own stop:
// force-deleted, evicted, or lost with its node. The kubelet never gets to
// report such a loss, so without this docker wait, an attached docker run
// and compose depends_on: service_completed_successfully hang forever even
// though inspect already shows the container exited (D2K-026, D2K-035). 137
// is Docker's own convention for a killed container.
func podLostEvent(known map[string]*appsv1.Deployment, terminations map[string]int32, app, podName string, now time.Time) []events.Message {
	d, ok := known[app]
	if !ok || !runsAsPod(*d) || d.Annotations[types.AnnotationStoppedAt] != "" || terminations[podName] != 0 {
		return nil
	}
	return []events.Message{containerEvent(d, events.ActionDie, now, map[string]string{"exitCode": "137"})}
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
