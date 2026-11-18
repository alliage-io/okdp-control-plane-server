package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	corev1 "k8s.io/api/core/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

// GetProjectMetrics aggregates live CPU/memory usage from metrics-server for
// every instance of a project, against the total limits read from the
// pods' container specs.
func (s *DefaultServiceService) GetProjectMetrics(ctx context.Context, project string) (map[string]*models.ServiceMetrics, error) {
	instances, err := s.ListServices(ctx, project)
	if err != nil {
		return nil, err
	}

	allPods, err := s.workloads.ListPods(ctx, project, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	usage := s.podUsage(ctx, project)

	metrics := make(map[string]*models.ServiceMetrics, len(instances))
	for _, inst := range instances {
		var pods []corev1.Pod
		for i := range allPods {
			if allPods[i].Labels[repository.LabelAppInstance] == inst.ReleaseName {
				pods = append(pods, allPods[i])
			}
		}
		metrics[inst.Name] = buildServiceMetrics(pods, usage)
	}
	return metrics, nil
}

// GetServiceMetrics aggregates the same for one instance.
func (s *DefaultServiceService) GetServiceMetrics(ctx context.Context, project, serviceName string) (*models.ServiceMetrics, error) {
	pods, err := s.workloads.ListPods(ctx, project, instanceSelector(project, serviceName))
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	return buildServiceMetrics(pods, s.podUsage(ctx, project)), nil
}

// podUsage is what metrics-server reports of a namespace's pods, by pod;
// none when it does not answer.
func (s *DefaultServiceService) podUsage(ctx context.Context, namespace string) map[string]*metricsv1beta1.PodMetrics {
	byName := map[string]*metricsv1beta1.PodMetrics{}
	list, err := s.workloads.PodMetrics(ctx, namespace)
	if err != nil {
		slog.Debug(fmt.Sprintf("metrics for namespace %s unavailable: %v", namespace, err))
		return byName
	}
	for i := range list {
		byName[list[i].Name] = &list[i]
	}
	return byName
}

func buildServiceMetrics(pods []corev1.Pod, usageByPod map[string]*metricsv1beta1.PodMetrics) *models.ServiceMetrics {
	var cpuLimit, memLimit float64
	var cpuUsed, memUsed float64
	cpuUsedAvailable := false
	memUsedAvailable := false

	for i := range pods {
		for c := range pods[i].Spec.Containers {
			resources := &pods[i].Spec.Containers[c].Resources
			// The limits when set, else the requests.
			quantities := resources.Limits
			if quantities == nil {
				quantities = resources.Requests
			}
			if cpu, ok := quantities[corev1.ResourceCPU]; ok {
				cpuLimit += cpu.AsApproximateFloat64()
			}
			if memory, ok := quantities[corev1.ResourceMemory]; ok {
				memLimit += memory.AsApproximateFloat64()
			}
		}
	}

	for i := range pods {
		podMetrics, ok := usageByPod[pods[i].Name]
		if !ok {
			continue
		}
		for c := range podMetrics.Containers {
			usage := podMetrics.Containers[c].Usage
			if cpu, ok := usage[corev1.ResourceCPU]; ok {
				cpuUsed += cpu.AsApproximateFloat64()
				cpuUsedAvailable = true
			}
			if memory, ok := usage[corev1.ResourceMemory]; ok {
				memUsed += memory.AsApproximateFloat64()
				memUsedAvailable = true
			}
		}
	}

	return &models.ServiceMetrics{
		CPU: models.MetricValue{
			UsedRaw:   cpuUsed,
			LimitRaw:  cpuLimit,
			Used:      formatCPU(cpuUsed),
			Limit:     formatCPU(cpuLimit),
			Pct:       ratio(cpuUsed, cpuLimit),
			Available: cpuUsedAvailable,
		},
		Memory: models.MetricValue{
			UsedRaw:   memUsed,
			LimitRaw:  memLimit,
			Used:      formatMemory(memUsed),
			Limit:     formatMemory(memLimit),
			Pct:       ratio(memUsed, memLimit),
			Available: memUsedAvailable,
		},
	}
}

// formatCPU returns a compact human-readable string in cores.
func formatCPU(cores float64) string {
	if cores == 0 {
		return "0"
	}
	if cores < 1 {
		return fmt.Sprintf("%.3f", cores)
	}
	return fmt.Sprintf("%.2f", cores)
}

// formatMemory picks the right binary unit for a byte value.
func formatMemory(bytes float64) string {
	if bytes == 0 {
		return "0"
	}
	units := []struct {
		threshold float64
		suffix    string
	}{
		{1024 * 1024 * 1024, "Gi"},
		{1024 * 1024, "Mi"},
		{1024, "Ki"},
	}
	for _, u := range units {
		if bytes >= u.threshold {
			return fmt.Sprintf("%.2f%s", bytes/u.threshold, u.suffix)
		}
	}
	return fmt.Sprintf("%.0fB", bytes)
}

func ratio(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	r := a / b
	if r > 1 {
		return 1
	}
	return r
}
