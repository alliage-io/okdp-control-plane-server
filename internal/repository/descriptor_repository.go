package repository

import (
	"context"
	"sort"
	"time"

	"github.com/okdp/okdp-gitops/gitops"
)

// LabelAppInstance is the label every workload of an instance carries:
// app.kubernetes.io/instance: <release>.
const LabelAppInstance = "app.kubernetes.io/instance"

// Descriptor is what a compiled instance publishes: its okdp.info.yaml
// in the output of the deployments layout (url, usage, the connections it
// provides), and the package of the Chart.yaml beside it.
type Descriptor struct {
	// Release is the Helm release name (<project>-<instance>).
	Release   string
	Namespace string
	Service   string
	Version   string
	URL       string
	Usage     string
	Outputs   []DescriptorOutput
	// CreatedAt is unknown in Git: zero.
	CreatedAt time.Time
}

// DescriptorOutput is a connection an instance provides.
type DescriptorOutput struct {
	Name     string         `json:"name"`
	Contract string         `json:"contract"`
	Values   map[string]any `json:"values,omitempty"`
	// SecretRef names the Secret, in the instance namespace, holding the
	// secret fields of the contract.
	SecretRef struct {
		Name string `json:"name"`
	} `json:"secretRef"`
}

// DescriptorRepository reads what the instances of a project publish.
type DescriptorRepository interface {
	List(ctx context.Context, namespace string) ([]Descriptor, error)
}

// publishedRepository reads compiled/projects/<p>/services/<i>/okdp.info.yaml
// in the output of the deployments layout.
type publishedRepository struct {
	deployments *gitops.Deployments
}

// NewDescriptorRepository reads what the compiled instances publish, in the
// output of deployments.
func NewDescriptorRepository(deployments *gitops.Deployments) DescriptorRepository {
	return &publishedRepository{deployments: deployments}
}

func (r *publishedRepository) List(ctx context.Context, namespace string) ([]Descriptor, error) {
	published, err := r.deployments.ListPublished(ctx, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]Descriptor, 0, len(published))
	for _, p := range published {
		out = append(out, DescriptorFromPublished(namespace, p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release < out[j].Release })
	return out, nil
}

// DescriptorFromPublished is the descriptor of what an instance publishes.
func DescriptorFromPublished(namespace string, p gitops.Published) Descriptor {
	d := Descriptor{
		Release:   p.Release,
		Namespace: namespace,
		Service:   p.Service,
		Version:   p.Version,
		URL:       p.Publishes.URL,
		Usage:     p.Publishes.Usage,
	}
	for _, o := range p.Publishes.Provides {
		out := DescriptorOutput{Name: o.Name, Contract: o.Contract, Values: o.Values}
		if o.SecretRef != nil {
			out.SecretRef.Name = o.SecretRef.Name
		}
		d.Outputs = append(d.Outputs, out)
	}
	return d
}
