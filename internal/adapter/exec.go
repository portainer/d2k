package adapter

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/scheme"
)

// ExecOptions mirrors the subset of docker exec flags d2k supports.
type ExecOptions struct {
	// Name is the container name or ID.
	Name string
	// Cmd is the command to run.
	Cmd []string
	// AttachStdin enables stdin.
	AttachStdin bool
	// AttachStdout enables stdout.
	AttachStdout bool
	// AttachStderr enables stderr.
	AttachStderr bool
	// Tty allocates a pseudo-TTY.
	Tty bool
}

// ExecContainer executes a command in the container's pod.
func (a *KubernetesDockerAdapter) ExecContainer(ctx context.Context, opts ExecOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	resolved, err := a.resolveDeploymentName(ctx, opts.Name)
	if err != nil {
		return err
	}

	pod, err := a.currentPodForDeployment(ctx, resolved)
	if err != nil {
		return err
	}

	// Find the container name in the pod — use the first container.
	containerName := resolved
	if len(pod.Spec.Containers) > 0 {
		containerName = pod.Spec.Containers[0].Name
	}

	req := a.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(a.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   opts.Cmd,
			Stdin:     opts.AttachStdin,
			Stdout:    opts.AttachStdout,
			Stderr:    opts.AttachStderr,
			TTY:       opts.Tty,
		}, scheme.ParameterCodec)

	restCfg, err := buildRestConfig("")
	if err != nil {
		return fmt.Errorf("unable to build REST config for exec: %w", err)
	}

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("unable to create SPDY executor: %w", err)
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    opts.Tty,
	})
}
