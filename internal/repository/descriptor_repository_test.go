package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-gitops/gitops"
	"github.com/stretchr/testify/require"
)

// What an instance publishes is its okdp.instance.yaml in the output.
func TestDescriptorsAreWhatTheCompiledInstancesPublish(t *testing.T) {
	store := gitops.NewMemoryStore(map[string]string{
		"compiled/projects/demo/services/hive/Chart.yaml":         "apiVersion: v2\nname: hive-metastore\nversion: 4.0.1-1.0.0\n",
		"compiled/projects/demo/services/hive/okdp.instance.yaml": "usage: |\n  # Hive\nprovides:\n  - name: demo-hive\n    contract: hive\n    values:\n      thriftUri: thrift://demo-hive-hive-metastore.demo.svc:9083\n    secretRef:\n      name: demo-hive-hive-credentials\n",
		"compiled/projects/demo/services/web/Chart.yaml":          "apiVersion: v2\nname: webapp\nversion: 1.0.0\n",
		"compiled/projects/demo/services/web/okdp.instance.yaml":  "url: https://demo-web.example.org\n",
	})
	list, err := NewDescriptorRepository(gitops.NewDeployments(store, nil)).List(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, list, 2)
	hive := list[0]
	require.Equal(t, "demo-hive", hive.Release)
	require.Equal(t, "demo", hive.Namespace)
	require.Equal(t, "hive-metastore", hive.Service)
	require.Equal(t, "4.0.1-1.0.0", hive.Version)
	require.Equal(t, "# Hive\n", hive.Usage)
	require.Len(t, hive.Outputs, 1)
	require.Equal(t, "hive", hive.Outputs[0].Contract)
	require.Equal(t, "demo-hive-hive-credentials", hive.Outputs[0].SecretRef.Name)
	require.Equal(t, "thrift://demo-hive-hive-metastore.demo.svc:9083", hive.Outputs[0].Values["thriftUri"])
	require.Equal(t, "https://demo-web.example.org", list[1].URL)
	require.Empty(t, list[1].Outputs)

	none, err := NewDescriptorRepository(gitops.NewDeployments(store, nil)).List(context.Background(), "other")
	require.NoError(t, err)
	require.Empty(t, none)
}
