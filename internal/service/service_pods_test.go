package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

func TestPodModelLeavesOutTheSidecars(t *testing.T) {
	s := &DefaultServiceService{sidecarPrefixes: []string{"istio-"}}
	got := s.podModel(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour))},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Image: "img", Ready: true, RestartCount: 2},
				{Name: "worker", Image: "img2", RestartCount: 1},
				{Name: "istio-proxy", Ready: true, RestartCount: 9},
			},
		},
	})
	assert.Equal(t, models.Pod{
		Name: "p", Status: "Running", Age: "3h", Ready: "1/2", Restarts: 3,
		Containers: []models.Container{{Name: "app", Image: "img", Ready: true}, {Name: "worker", Image: "img2"}},
	}, got)
}

func TestCheckPodHealth(t *testing.T) {
	s := &DefaultServiceService{sidecarPrefixes: []string{"istio-"}}
	pod := func(release string, statuses ...corev1.ContainerStatus) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{repository.LabelAppInstance: release}},
			Status:     corev1.PodStatus{ContainerStatuses: statuses},
		}
	}
	waiting := func(name, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
	}
	for name, tc := range map[string]struct {
		pod  corev1.Pod
		want string
	}{
		"crash loop":          {pod("r", waiting("app", "CrashLoopBackOff")), "Error"},
		"image pull":          {pod("r", waiting("app", "ImagePullBackOff")), "Error"},
		"still creating":      {pod("r", waiting("app", "ContainerCreating")), "Ready"},
		"a sidecar failing":   {pod("r", waiting("istio-proxy", "CrashLoopBackOff")), "Ready"},
		"another release":     {pod("other", waiting("app", "CrashLoopBackOff")), "Ready"},
		"a ready container":   {pod("r", corev1.ContainerStatus{Name: "app", Ready: true}), "Ready"},
		"terminated on error": {pod("r", corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error"}}}), "Error"},
	} {
		assert.Equal(t, tc.want, s.checkPodHealth([]corev1.Pod{tc.pod}, "r", "Ready"), name)
	}
}

func TestEventTimestampPrefersLastTimestamp(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	older := now.Add(-time.Hour)
	assert.True(t, eventTimestamp(&corev1.Event{LastTimestamp: metav1.NewTime(now), EventTime: metav1.NewMicroTime(older)}).Equal(now))
	assert.True(t, eventTimestamp(&corev1.Event{EventTime: metav1.NewMicroTime(now)}).Equal(now))
	assert.True(t, eventTimestamp(&corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(now)}}).Equal(now))
}

func TestBuildServiceMetrics(t *testing.T) {
	quantity := resource.MustParse
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "with-limits"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{corev1.ResourceCPU: quantity("500m"), corev1.ResourceMemory: quantity("1Gi")},
				Requests: corev1.ResourceList{corev1.ResourceCPU: quantity("9")},
			}}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "with-requests"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: quantity("1500m")},
			}}}},
		},
	}
	usage := map[string]*metricsv1beta1.PodMetrics{"with-limits": {Containers: []metricsv1beta1.ContainerMetrics{{
		Usage: corev1.ResourceList{corev1.ResourceCPU: quantity("1500000000n")},
	}}}}

	m := buildServiceMetrics(pods, usage)
	assert.InDelta(t, 2.0, m.CPU.LimitRaw, 1e-9)
	assert.InDelta(t, 1<<30, m.Memory.LimitRaw, 1)
	assert.InDelta(t, 1.5, m.CPU.UsedRaw, 1e-9)
	assert.True(t, m.CPU.Available)
	assert.False(t, m.Memory.Available, "no memory usage reported")
}
