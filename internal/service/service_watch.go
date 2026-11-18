package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-control-plane-server/internal/apperr"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// --- Live stream ---

func (s *DefaultServiceService) WatchServices(ctx context.Context, project string) (<-chan ServiceEvent, error) {
	ctx, cancel := context.WithCancel(ctx)
	out := make(chan ServiceEvent, 16)

	type trigger struct {
		instance  string
		eventType string
	}
	triggers := make(chan trigger, 64)
	forward := func(instance, eventType string) {
		select {
		case triggers <- trigger{instance, eventType}:
		case <-ctx.Done():
		}
	}

	started := 0
	prefix := project + "-"
	if s.engine != nil {
		if changes, err := s.engine.Watch(ctx); err != nil {
			slog.Warn("Could not watch the GitOps engine objects", "error", err, "engine", s.engine.Name())
		} else {
			started++
			go func() {
				defer cancel()
				for change := range changes {
					if change.Project == project && strings.HasPrefix(change.Release, prefix) {
						forward(strings.TrimPrefix(change.Release, prefix), change.Type)
					}
				}
			}()
		}
	}
	commits, unsubscribe := s.changes.Subscribe(project)
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-commits:
				forward(c.Instance, c.Type)
			}
		}
	}()
	if started == 0 {
		slog.Warn("Streaming the services of a project from the console's own commits only", "project", project)
	}

	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-triggers:
				event, ok := s.eventFor(ctx, project, t.instance, t.eventType)
				if !ok {
					continue
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// eventFor recomputes an instance after a change. The declaration decides
// whether it still exists: an engine object deleted while Git still declares the
// instance is a change of status, not a deletion.
func (s *DefaultServiceService) eventFor(ctx context.Context, project, name, eventType string) (ServiceEvent, bool) {
	instance, err := s.GetService(ctx, project, name)
	if errors.Is(err, apperr.ErrNotFound) {
		return ServiceEvent{Type: "DELETED", Object: models.ServiceInstance{
			Name: name, ReleaseName: compiler.ReleaseName(project, name), TargetNamespace: project,
		}}, true
	}
	if err != nil {
		slog.Debug("Could not refresh a streamed instance", "error", err, "instance", project+"/"+name)
		return ServiceEvent{}, false
	}
	if eventType != "ADDED" {
		eventType = "MODIFIED"
	}
	return ServiceEvent{Type: eventType, Object: *instance}, true
}
