package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-gitops/gitops"
	"github.com/okdp/okdp-gitops/treeedit"
	"gopkg.in/yaml.v3"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// ChartFiles reads a file of an upstream chart (its values.yaml), from the
// chart cache, pulled first when the cache lacks it (chartcache.Cache.File).
type ChartFiles interface {
	File(ctx context.Context, ref compiler.ChartRef, name string) ([]byte, error)
}

// RenderedValuesService shows what each chart an instance deploys is
// rendered with: the chart's defaults coalesced under its compiled values,
// as Helm renders it, with the lines set by OKDP or the instance.
type RenderedValuesService interface {
	GetRenderedValues(ctx context.Context, project, name string) ([]models.RenderedValues, error)
}

type DefaultRenderedValuesService struct {
	deployments *gitops.Deployments
	charts      ChartFiles
}

func NewDefaultRenderedValuesService(deployments *gitops.Deployments, charts ChartFiles) *DefaultRenderedValuesService {
	return &DefaultRenderedValuesService{deployments: deployments, charts: charts}
}

// umbrellaDependency is a dependency of a compiled umbrella's Chart.yaml.
type umbrellaDependency struct {
	Name       string `yaml:"name"`
	Alias      string `yaml:"alias"`
	Repository string `yaml:"repository"`
	Version    string `yaml:"version"`
}

func (d umbrellaDependency) key() string {
	if d.Alias != "" {
		return d.Alias
	}
	return d.Name
}

// GetRenderedValues lists the charts of an instance with the values each is
// rendered with: each dependency of its compiled umbrella (the key named
// like the service first, then by key), its compiled values under that
// key. An instance that was not compiled has none. A chart whose defaults cannot be
// fetched keeps its compiled values only (DefaultsError), it does not fail
// the request.
func (s *DefaultRenderedValuesService) GetRenderedValues(ctx context.Context, project, name string) ([]models.RenderedValues, error) {
	inst, err := s.deployments.GetInstance(ctx, project, name)
	if err != nil {
		return nil, gitError(err, name)
	}
	compiled, err := s.deployments.ReadCompiled(ctx, project, name)
	if errors.Is(err, treeedit.ErrNotFound) {
		return []models.RenderedValues{}, nil
	}
	if err != nil {
		return nil, err
	}

	var umbrella struct {
		Dependencies []umbrellaDependency `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(compiled.Chart, &umbrella); err != nil {
		return nil, fmt.Errorf("the compiled Chart.yaml of %s/%s: %w", project, name, err)
	}
	deps := umbrella.Dependencies
	sort.SliceStable(deps, func(i, j int) bool {
		if (deps[i].key() == inst.Service) != (deps[j].key() == inst.Service) {
			return deps[i].key() == inst.Service
		}
		return deps[i].key() < deps[j].key()
	})
	result := make([]models.RenderedValues, 0, len(deps))
	for _, d := range deps {
		// The instance's objects, not the values of an upstream chart.
		if d.key() == compiler.ObjectsChart {
			continue
		}
		values, _ := compiled.Values[d.key()].(map[string]any)
		ref := compiler.ChartRef{Repository: d.Repository, Chart: d.Name, Version: d.Version}
		rendered := models.RenderedValues{
			Name:           d.key(),
			Chart:          d.Name,
			ChartVersion:   d.Name + "-" + d.Version,
			ServiceVersion: inst.Version,
		}
		if err := s.fill(ctx, &rendered, ref, values); err != nil {
			return nil, err
		}
		result = append(result, rendered)
	}
	return result, nil
}

// fill sets the values, the defaults and the changed lines of rendered: the
// chart's defaults coalesced under the compiled values, as Helm renders the
// chart; the compiled values alone when the defaults cannot be read.
func (s *DefaultRenderedValuesService) fill(ctx context.Context, rendered *models.RenderedValues, ref compiler.ChartRef, values map[string]any) error {
	rendered.ChangedLines = []int{}
	compiledOnly := func(reason string) error {
		encoded, err := treeedit.EncodeValues(values)
		if err != nil {
			return err
		}
		rendered.Values = string(encoded)
		rendered.DefaultsError = reason
		return nil
	}
	if s.charts == nil {
		return compiledOnly("no chart cache: the defaults of " + ref.Chart + " are not read")
	}
	defaults, err := s.charts.File(ctx, ref, "values.yaml")
	if err != nil {
		return compiledOnly(fmt.Sprintf("could not read the defaults of %s %s from %s: %v", ref.Chart, ref.Version, ref.Repository, err))
	}
	rendered.Defaults = string(defaults)
	parsed, err := compiler.ParseValues(defaults)
	if err != nil {
		return compiledOnly("the defaults do not parse: " + err.Error())
	}
	encoded, err := treeedit.EncodeValues(compiler.CoalesceValues(values, parsed))
	if err != nil {
		return err
	}
	rendered.Values = string(encoded)
	if lines, err := ChangedLines(rendered.Values, defaults); err != nil {
		rendered.DefaultsError = "could not compare with the defaults: " + err.Error()
	} else {
		rendered.ChangedLines = lines
	}
	return nil
}
