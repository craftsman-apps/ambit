package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NotReadyError lists, one line per pod, why the workloads did not become ready in time.
type NotReadyError struct {
	Timeout time.Duration
	Pods    []string
}

func (e *NotReadyError) Error() string {
	return fmt.Sprintf("not ready after %s: %s", e.Timeout, strings.Join(e.Pods, "; "))
}

// WaitWorkloads waits until every Deployment and StatefulSet in ns has all its replicas ready, and each label
// selector in required matches at least one ready Deployment (the gateway's Deployment is created by agentgateway's
// controller, not by Helm, so it can be missing at first). Every `every` it calls progress with the pods not yet
// ready. On timeout it returns a *NotReadyError with each not-ready pod and its reason (spec 4.1 step 4).
func WaitWorkloads(ctx context.Context, cs kubernetes.Interface, ns string, required []string, every, timeout time.Duration, progress func(string)) error {
	deadline := time.Now().Add(timeout)
	for {
		// When the caller's own deadline ended the wait (install --timeout), the reasons are still read, with a
		// short context of their own: the error must name the cause, not "context deadline exceeded" (#108).
		expired := errors.Is(ctx.Err(), context.DeadlineExceeded)
		if expired {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
		}
		unmet, err := workloadsReady(ctx, cs, ns, required)
		lastErr := err
		if err == nil && len(unmet) == 0 {
			return nil
		}
		var waiting []corev1.Pod
		pods, perr := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if perr != nil {
			lastErr = perr
		} else {
			for _, p := range pods.Items {
				if !podReady(&p) && p.Status.Phase != corev1.PodSucceeded {
					waiting = append(waiting, p)
				}
			}
		}
		if expired || time.Now().After(deadline) {
			e := &NotReadyError{Timeout: timeout}
			for _, p := range waiting {
				e.Pods = append(e.Pods, p.Name+": "+PodReason(&p, func(pod, container string) string { return lastLogLine(ctx, cs, ns, pod, container) }))
			}
			e.Pods = append(e.Pods, unmet...)
			if lastErr != nil {
				e.Pods = append(e.Pods, "last API error: "+sanitize(lastErr.Error()))
			}
			return e
		}
		names := make([]string, 0, len(waiting))
		for _, p := range waiting {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		// The pods first, then every workload line: a waiting pod must not hide a workload with no pod at all.
		parts := names
		if len(names) > 0 {
			parts = []string{strings.Join(names, ", ")}
		}
		parts = append(parts, unmet...)
		if len(parts) > 0 {
			progress("waiting for " + strings.Join(parts, "; "))
		} else {
			progress("waiting: API error: " + sanitize(lastErr.Error()))
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				continue // the next round reads the reasons and returns them
			}
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// workloadsReady returns one line for each workload that is not complete, and the last API error. A list that
// fails does not drop what the other lists found (#108).
func workloadsReady(ctx context.Context, cs kubernetes.Interface, ns string, required []string) ([]string, error) {
	var unmet []string
	var lastErr error
	deps, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		lastErr = err
	} else {
		for _, d := range deps.Items {
			if !deploymentReady(&d) {
				unmet = append(unmet, deploymentLine(&d))
			}
		}
	}
	sts, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return unmet, err
	}
	for _, s := range sts.Items {
		if want := replicas(s.Spec.Replicas); s.Status.ReadyReplicas < want {
			unmet = append(unmet, fmt.Sprintf("StatefulSet %s: %d/%d ready", s.Name, s.Status.ReadyReplicas, want))
		}
	}
	for _, sel := range required {
		l, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, err
		}
		if len(l.Items) == 0 {
			unmet = append(unmet, "no Deployment matches "+sel+" (missing)")
		}
	}
	return unmet, lastErr
}

// deploymentLine says why a Deployment is not complete. During a rollout the old pods are still Ready, so the
// ready count alone ("1/1 ready") would not explain the wait (#108).
func deploymentLine(d *appsv1.Deployment) string {
	want := replicas(d.Spec.Replicas)
	switch {
	case d.Status.ObservedGeneration < d.Generation:
		return fmt.Sprintf("Deployment %s: the controller has not seen the new spec yet", d.Name)
	case d.Status.UpdatedReplicas < want && d.Status.ReadyReplicas > 0:
		return fmt.Sprintf("Deployment %s: %d/%d ready, %d/%d updated (rollout in progress)", d.Name, d.Status.ReadyReplicas, want, d.Status.UpdatedReplicas, want)
	}
	return fmt.Sprintf("Deployment %s: %d/%d ready", d.Name, d.Status.ReadyReplicas, want)
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

func deploymentReady(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	// ObservedGeneration: until the controller has seen the new spec, the counts are those of the old rollout.
	return d.Status.ObservedGeneration >= d.Generation && d.Status.ReadyReplicas >= want && d.Status.UpdatedReplicas >= want
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodReason says in one line why a pod is not ready: ImagePullBackOff with the message, Pending with the
// scheduler's message, CrashLoopBackOff with the last log line. lastLog returns the last log line of a container.
func PodReason(p *corev1.Pod, lastLog func(pod, container string) string) string {
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return sanitize(w.Reason + ": " + w.Message)
			case "CrashLoopBackOff":
				return sanitize("CrashLoopBackOff, last log line: " + lastLog(p.Name, cs.Name))
			case "ContainerCreating", "PodInitializing":
			default:
				return sanitize(w.Reason + ": " + w.Message)
			}
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return sanitize("Pending: " + c.Message)
		}
	}
	return sanitize(string(p.Status.Phase) + ", not ready yet")
}

func lastLogLine(ctx context.Context, cs kubernetes.Interface, ns, pod, container string) string {
	tail := int64(1)
	b, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: true, TailLines: &tail}).DoRaw(ctx)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	return string(b)
}

// sanitize makes one printable line of at most 200 characters: control characters (terminal escape sequences,
// new lines) become spaces, and runs of spaces become one.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:199]) + "…"
	}
	return s
}
