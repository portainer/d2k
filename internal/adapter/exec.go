package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/portainer/d2k/internal/errdefs"
)

// ExecOptions mirrors the subset of docker exec flags d2k supports.
type ExecOptions struct {
	Name         string
	Cmd          []string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
}

// ExecStreams connects an exec to its client. Unset streams are not attached.
type ExecStreams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Resize remotecommand.TerminalSizeQueue
}

// ExecContainer runs a command in the container's pod and returns its exit
// code. An error means the command could not be run or streamed.
func (a *KubernetesDockerAdapter) ExecContainer(ctx context.Context, opts ExecOptions, streams ExecStreams) (int, error) {
	resolved, err := a.resolveDeploymentName(ctx, opts.Name)
	if err != nil {
		return 0, err
	}
	pod, err := a.currentPodForDeployment(ctx, resolved)
	if err != nil {
		return 0, err
	}
	if pod.Status.Phase != corev1.PodRunning {
		return 0, errdefs.Conflict(fmt.Errorf("container %s is not running", opts.Name))
	}

	req := a.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(a.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: pod.Spec.Containers[0].Name,
			Command:   opts.Cmd,
			Stdin:     streams.Stdin != nil,
			Stdout:    streams.Stdout != nil,
			Stderr:    streams.Stderr != nil && !opts.Tty,
			TTY:       opts.Tty,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(a.restConfig, "POST", req.URL())
	if err != nil {
		return 0, fmt.Errorf("unable to create SPDY executor: %w", err)
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             streams.Stdin,
		Stdout:            streams.Stdout,
		Stderr:            streams.Stderr,
		Tty:               opts.Tty,
		TerminalSizeQueue: streams.Resize,
	})
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus(), nil
	}
	return 0, err
}
