package repository

import (
	"fmt"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/okdp/okdp-control-plane-server/internal/buildinfo"
)

// Clients are the clients of the cluster, all from one rest.Config.
type Clients struct {
	// Dynamic reaches the custom resources: the engines' objects, SecretStores,
	// ExternalSecrets, SparkApplications.
	Dynamic dynamic.Interface
	// Typed reaches the core API: namespaces, secrets, pods, events, PVCs.
	Typed kubernetes.Interface
	// Discovery tells whether an optional CRD is installed before it is used.
	Discovery discovery.DiscoveryInterface
	// Metrics reaches metrics-server (metrics.k8s.io).
	Metrics metricsclient.Interface
}

// NewClients opens the clients of the cluster (in-cluster or local kubeconfig).
func NewClients() (*Clients, error) {
	config, err := restConfig()
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}
	typedClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create typed client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}
	metrics, err := metricsclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create metrics client: %w", err)
	}
	return &Clients{Dynamic: dynamicClient, Typed: typedClient, Discovery: discoveryClient, Metrics: metrics}, nil
}

// restConfig returns the one Kubernetes REST config of the server, named in
// the API server's audit log by its user agent.
func restConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

		config, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get kubernetes config (tried in-cluster and local): %w", err)
		}
	}
	config.QPS = 50
	config.Burst = 100
	name, version := buildinfo.Program("okdp-control-plane-server")
	return rest.AddUserAgent(config, name+"/"+version), nil
}
