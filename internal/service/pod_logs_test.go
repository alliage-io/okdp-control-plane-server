package service

import (
	"context"
	"errors"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

func pod(name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo", Labels: labels}}
}

// The logs route names a service: only the pods of that instance are readable
// through it, the ones ListPods shows.
func TestPodLogsAreServedOnlyForThePodsOfTheInstance(t *testing.T) {
	svc := NewDefaultServiceService(ServiceDeps{Workloads: repository.NewWorkloads(k8sfake.NewSimpleClientset(
		pod("trino-coordinator-0", map[string]string{repository.LabelAppInstance: "demo-trino"}),
		pod("hive-metastore-0", map[string]string{repository.LabelAppInstance: "demo-hive"}),
		pod("stray", nil),
	), nil)})
	ctx := context.Background()

	stream, err := svc.GetPodLogs(ctx, "demo", "trino", "trino-coordinator-0", "", 100, false)
	if err != nil {
		t.Fatalf("own pod: %v", err)
	}
	_, _ = io.ReadAll(stream)
	stream.Close()

	for _, name := range []string{"hive-metastore-0", "stray", "absent"} {
		if _, err := svc.GetPodLogs(ctx, "demo", "trino", name, "", 100, false); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("pod %s through service trino: %v, want NotFound", name, err)
		}
	}
}
