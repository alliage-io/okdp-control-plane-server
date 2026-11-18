package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	corev1 "k8s.io/api/core/v1"
)

// --- Pods ---

// instanceSelector selects the workloads of an instance: every one carries
// app.kubernetes.io/instance: <project>-<instance> (chart rules).
func instanceSelector(project, name string) string {
	return fmt.Sprintf("%s=%s", repository.LabelAppInstance, compiler.ReleaseName(project, name))
}

func (s *DefaultServiceService) ListPods(ctx context.Context, project, serviceName string) ([]models.Pod, error) {
	pods, err := s.workloads.ListPods(ctx, project, instanceSelector(project, serviceName))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	result := []models.Pod{}
	for i := range pods {
		result = append(result, s.podModel(&pods[i]))
	}
	return result, nil
}

// ownsByName reports whether an object named by a release belongs to
// releaseName rather than to one of the other releases still living in the
// namespace. Names are the only link available here (the singleuser objects
// JupyterHub creates carry no ownerReference back to the Release), and a bare
// prefix test is not enough: `demo-jupyter-` also matches
// `demo-jupyter-ds-claim-alice`, the home volume of a user of the *other*
// instance. Whichever prefix is the longest match wins, so an object is only
// ever claimed by the release whose name reaches furthest into it.
func ownsByName(objectName, releaseName string, otherReleases []string) bool {
	prefix := releaseName + "-"
	if len(objectName) <= len(prefix) || objectName[:len(prefix)] != prefix {
		return false
	}
	for _, other := range otherReleases {
		if len(other) <= len(releaseName) {
			continue
		}
		// The neighbour's own name is one of its objects too: a chart names its
		// PVC after the release itself.
		if objectName == other {
			return false
		}
		otherPrefix := other + "-"
		if len(objectName) > len(otherPrefix) && objectName[:len(otherPrefix)] == otherPrefix {
			return false
		}
	}
	return true
}

// cleanupUserResources removes the pods and volumes a release created outside
// its own manifests, JupyterHub's per-user servers and home volumes, which no
// controller reclaims when the release goes.
//
// Deleting a volume is irreversible, so this fails closed: if the surviving
// releases cannot be listed, nothing is deleted rather than everything matching
// a prefix.
func (s *DefaultServiceService) cleanupUserResources(ctx context.Context, namespace, releaseName string) {
	if s.workloads == nil {
		return
	}
	// Called after the instance has been removed from Git, so this lists the neighbours.
	survivors, err := s.deployments.ListInstances(ctx, namespace)
	if err != nil {
		slog.Error("Skipping user resource cleanup: cannot tell this release's objects from its neighbours'", "error", err, "release", releaseName)
		return
	}
	others := make([]string, 0, len(survivors))
	for i := range survivors {
		if name := survivors[i].Release(); name != releaseName {
			others = append(others, name)
		}
	}

	for _, target := range []struct {
		kind   string
		list   func(context.Context, string) ([]string, error)
		remove func(context.Context, string, string) error
	}{
		{"pod", s.workloads.ListPodNames, s.workloads.DeletePod},
		{"pvc", s.workloads.ListPVCNames, s.workloads.DeletePVC},
	} {
		names, err := target.list(ctx, namespace)
		if err != nil {
			slog.Warn("Could not list user resources to clean up", "error", err, "kind", target.kind)
			continue
		}
		for _, name := range names {
			if !ownsByName(name, releaseName, others) {
				continue
			}
			if err := target.remove(ctx, namespace, name); err != nil {
				slog.Warn("Could not clean up user resource", "error", err, target.kind, name)
				continue
			}
			slog.Info("Cleaned up user resource", target.kind, name)
		}
	}
}

// checkPodHealth is Error when a container of the release's pods, its
// infrastructure sidecars aside, is failing; otherwise currentStatus.
func (s *DefaultServiceService) checkPodHealth(pods []corev1.Pod, releaseName string, currentStatus string) string {
	for i := range pods {
		if pods[i].Labels[repository.LabelAppInstance] != releaseName {
			continue
		}
		for _, cs := range pods[i].Status.ContainerStatuses {
			if s.isInfraSidecar(cs.Name) || cs.Ready {
				continue
			}
			if waiting := cs.State.Waiting; waiting != nil {
				switch waiting.Reason {
				case "CrashLoopBackOff", "Error", "ImagePullBackOff", "ErrImagePull":
					return "Error"
				}
			}
			if terminated := cs.State.Terminated; terminated != nil && terminated.Reason == "Error" {
				return "Error"
			}
		}
	}
	return currentStatus
}

func (s *DefaultServiceService) isInfraSidecar(containerName string) bool {
	for _, prefix := range s.sidecarPrefixes {
		if strings.HasPrefix(containerName, prefix) {
			return true
		}
	}
	return false
}

func (s *DefaultServiceService) GetPodLogs(ctx context.Context, project, serviceName, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	// The pod must be one ListPods shows for this instance. Without the check,
	// any pod of the namespace is readable through any service name, sidecars
	// and other tenants' jobs included.
	pod, err := s.workloads.GetPod(ctx, project, podName)
	if err != nil {
		if errors.Is(err, apperr.ErrNotFound) {
			return nil, apperr.NotFound("pods", podName)
		}
		return nil, fmt.Errorf("failed to get pod: %w", err)
	}
	if pod.Labels[repository.LabelAppInstance] != compiler.ReleaseName(project, serviceName) {
		return nil, apperr.NotFound("pods", podName)
	}

	stream, err := s.workloads.PodLogs(ctx, project, podName, container, tailLines, follow)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod logs: %w", err)
	}
	return stream, nil
}

// podModel is the console's view of a pod, its infrastructure sidecars left
// out.
func (s *DefaultServiceService) podModel(p *corev1.Pod) models.Pod {
	pod := models.Pod{
		Name:   p.Name,
		Status: string(p.Status.Phase),
	}

	if !p.CreationTimestamp.IsZero() {
		d := time.Since(p.CreationTimestamp.Time)
		switch {
		case d.Hours() >= 24:
			pod.Age = fmt.Sprintf("%dd", int(d.Hours()/24))
		case d.Hours() >= 1:
			pod.Age = fmt.Sprintf("%dh", int(d.Hours()))
		default:
			pod.Age = fmt.Sprintf("%dm", int(d.Minutes()))
		}
	}

	readyCount := 0
	totalCount := 0
	var restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		if s.isInfraSidecar(cs.Name) {
			continue
		}
		totalCount++
		if cs.Ready {
			readyCount++
		}
		restarts += cs.RestartCount
		pod.Containers = append(pod.Containers, models.Container{
			Name:  cs.Name,
			Image: cs.Image,
			Ready: cs.Ready,
		})
	}

	pod.Ready = fmt.Sprintf("%d/%d", readyCount, totalCount)
	pod.Restarts = restarts

	return pod
}
