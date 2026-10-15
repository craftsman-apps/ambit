package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

// Forward opens a port-forward from a free port on 127.0.0.1 to port on the first ready pod that matches selector
// and returns that local port. stop ends it. It needs `create` on pods/portforward; it creates and changes no object.
func Forward(ctx context.Context, c *Clients, ns, selector string, port int) (local int, stop func(), err error) {
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return 0, nil, err
	}
	pod, err := pickPod(pods.Items)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: %w", selector, err)
	}
	transport, upgrader, err := spdy.RoundTripperFor(c.Config)
	if err != nil {
		return 0, nil, err
	}
	u, err := url.Parse(c.Config.Host)
	if err != nil {
		return 0, nil, err
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", ns, pod)
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	stopc, ready := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stopc, ready, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, err
	}
	errc := make(chan error, 1)
	go func() { errc <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-errc:
		return 0, nil, err
	case <-time.After(15 * time.Second):
		close(stopc)
		return 0, nil, fmt.Errorf("port-forward to %s did not start within 15s", pod)
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stopc)
		return 0, nil, fmt.Errorf("port-forward to %s: no local port: %v", pod, err)
	}
	return int(ports[0].Local), func() { close(stopc) }, nil
}

// pickPod returns the first running, ready pod.
func pickPod(pods []corev1.Pod) (string, error) {
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning && podReady(&pods[i]) && pods[i].DeletionTimestamp == nil {
			return pods[i].Name, nil
		}
	}
	return "", fmt.Errorf("no ready pod")
}

// Exec runs cmd in the first ready pod that matches selector and returns what it wrote to standard output. It
// needs `create` on pods/exec. Standard error goes into the error. The caller passes only read-only commands.
func Exec(ctx context.Context, c *Clients, ns, selector string, cmd []string) (string, error) {
	pods, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	pod, err := pickPod(pods.Items)
	if err != nil {
		return "", fmt.Errorf("%s: %w", selector, err)
	}
	req := c.Kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Command: cmd, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(c.Config, http.MethodPost, req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr strings.Builder
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return stdout.String(), nil
}
