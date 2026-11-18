package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-gitops/treeedit"
)

// --- Platform services ---

func (s *DefaultServiceService) GetPlatformServices(ctx context.Context) ([]models.PlatformService, error) {
	services, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, err
	}
	if s.schemaService == nil {
		return services, nil
	}

	// The versions are the catalogs': the directories of the service's
	// package. Repository names the catalog holding it.
	versions := s.schemaService.ListVersionsForServices(ctx, services)
	catalogs := s.schemaService.CatalogOf(ctx)
	for i, svc := range services {
		if tags, ok := versions[svc.Name]; ok {
			services[i].Versions = tags
			services[i].DefaultVersion = DefaultVersion(svc.DefaultVersion, tags)
		}
		services[i].Repository = catalogs[svc.Name]
	}

	return services, nil
}

// AddPlatformService validates and appends a new service to the catalog.
func (s *DefaultServiceService) AddPlatformService(ctx context.Context, svc models.PlatformService) (*models.PlatformService, error) {
	if s.catalogWriter == nil {
		return nil, fmt.Errorf("catalog management is not available")
	}
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	existing, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read catalog: %w", err)
	}
	for _, e := range existing {
		if e.Name == svc.Name {
			return nil, fmt.Errorf("%w: %q", ErrCatalogServiceExists, svc.Name)
		}
	}

	// The service a package of the catalogs, its default one of its
	// versions: the compile's check of catalogs.yaml (UpdateCatalog), a
	// refusal a 400 of its problems.
	if err := s.catalogWriter.AddPlatformService(ctx, svc); err != nil {
		return nil, catalogWriteError(err)
	}
	s.platformRepo.Invalidate()
	return &svc, nil
}

// UpdatePlatformService validates and replaces an existing catalog service.
func (s *DefaultServiceService) UpdatePlatformService(ctx context.Context, name string, svc models.PlatformService) (*models.PlatformService, error) {
	if s.catalogWriter == nil {
		return nil, fmt.Errorf("catalog management is not available")
	}
	svc.Name = name // the path is the source of truth for identity
	if err := normalizeAndValidateCatalogService(&svc); err != nil {
		return nil, err
	}

	if _, err := s.findCatalogService(ctx, name); err != nil {
		return nil, err
	}

	if err := s.catalogWriter.UpdatePlatformService(ctx, name, svc); err != nil {
		return nil, catalogWriteError(err)
	}
	s.platformRepo.Invalidate()
	return &svc, nil
}

// RemovePlatformService removes a service from the catalog.
func (s *DefaultServiceService) RemovePlatformService(ctx context.Context, name string) error {
	if s.catalogWriter == nil {
		return fmt.Errorf("catalog management is not available")
	}
	if _, err := s.findCatalogService(ctx, name); err != nil {
		return err
	}
	if err := s.catalogWriter.RemovePlatformService(ctx, name); err != nil {
		return err
	}
	s.platformRepo.Invalidate()
	return nil
}

// findCatalogService returns the catalog service by name or ErrCatalogServiceNotFound.
func (s *DefaultServiceService) findCatalogService(ctx context.Context, name string) (*models.PlatformService, error) {
	existing, err := s.platformRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read catalog: %w", err)
	}
	for i := range existing {
		if existing[i].Name == name {
			return &existing[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrCatalogServiceNotFound, name)
}

// catalogWriteError is a catalogs.yaml the compile refuses
// (gitops.Compiler.CheckCatalogs, a *treeedit.CompileError), a 400 of its
// problems; any other error as it is.
func catalogWriteError(err error) error {
	var compileErr *treeedit.CompileError
	if errors.As(err, &compileErr) {
		return DoesNotCompile(compileErr)
	}
	return err
}

// normalizeAndValidateCatalogService enforces the catalog rules. The
// versions are the catalogs': the ones given are checked, not stored; the
// default is optional (the highest version when unset). Returns an error
// wrapping ErrCatalogValidation.
func normalizeAndValidateCatalogService(svc *models.PlatformService) error {
	svc.Name = strings.TrimSpace(svc.Name)
	if svc.Name == "" {
		return fmt.Errorf("%w: name is required", ErrCatalogValidation)
	}
	if !compiler.IsName(svc.Name) {
		return fmt.Errorf("%w: name %q must be a lowercase DNS-style identifier", ErrCatalogValidation, svc.Name)
	}
	svc.Category = strings.TrimSpace(svc.Category)
	if svc.Category == "" {
		return fmt.Errorf("%w: category is required, it names the console section the service belongs to", ErrCatalogValidation)
	}
	seen := make(map[string]bool, len(svc.Versions))
	for _, v := range svc.Versions {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: version cannot be empty", ErrCatalogValidation)
		}
		if seen[v] {
			return fmt.Errorf("%w: duplicate version %q", ErrCatalogValidation, v)
		}
		seen[v] = true
	}
	if svc.DefaultVersion != "" && len(svc.Versions) > 0 && !seen[svc.DefaultVersion] {
		return fmt.Errorf("%w: default version %q must be one of the listed versions", ErrCatalogValidation, svc.DefaultVersion)
	}
	return nil
}

// --- Catalog (self-service) ---

func (s *DefaultServiceService) GetMenuCategories(ctx context.Context) ([]models.MenuCategory, error) {
	return s.platformRepo.GetMenuCategories(ctx)
}

func (s *DefaultServiceService) GetProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	return s.platformRepo.GetProfileImages(ctx)
}
