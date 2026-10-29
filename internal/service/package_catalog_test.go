package service

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/okdp/okdp-gitops/catalog"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// menuPlatform is a console menu of trino (default 480.0.0-1.0.0), webapp
// (no default) and gone (in no catalog).
type menuPlatform struct {
	repository.PlatformRepository
}

func (menuPlatform) GetPlatformServices(context.Context) ([]models.PlatformService, error) {
	return []models.PlatformService{{Name: "trino", DefaultVersion: "480.0.0-1.0.0"}, {Name: "webapp"}, {Name: "gone", DefaultVersion: "1.0.0"}}, nil
}

func testCatalogs(context.Context) (*catalog.Catalogs, error) {
	return catalog.New(catalog.Catalog{Name: "platform-charts", FS: fstest.MapFS{
		"catalog/trino/480.0.0-1.0.0/Chart.yaml":         {Data: []byte("name: trino\n")},
		"catalog/trino/481.0.0-1.0.0/Chart.yaml":         {Data: []byte("name: trino\n")},
		"catalog/trino/481.0.0-1.0.0/config.schema.json": {Data: []byte(`{"type": "object", "properties": {"workers": {"type": "integer"}, "global": {"type": "object"}, "hive": {"type": "string", "x-okdp-connection-ref": {"contract": "hive"}}}, "required": ["global"]}`)},
		"catalog/webapp/1.0.0/Chart.yaml":                {Data: []byte("name: webapp\n")},
		"catalog/webapp/1.1.0/Chart.yaml":                {Data: []byte("name: webapp\n")},
	}}), nil
}

func TestThePackagesComeFromTheCatalogs(t *testing.T) {
	ctx := context.Background()
	svc := NewDefaultPackageSchemaService(menuPlatform{}, testCatalogs)

	versions, err := svc.GetServiceVersions(ctx, "trino")
	require.NoError(t, err)
	require.Equal(t, &ServiceVersionsResponse{Versions: []string{"481.0.0-1.0.0", "480.0.0-1.0.0"}, Default: "480.0.0-1.0.0"}, versions)
	versions, err = svc.GetServiceVersions(ctx, "webapp")
	require.NoError(t, err)
	require.Equal(t, "1.1.0", versions.Default, "no default in the menu: the highest")
	versions, err = svc.GetServiceVersions(ctx, "gone")
	require.NoError(t, err)
	require.Equal(t, []string{"1.0.0"}, versions.Versions, "in no catalog: the menu's default")
	_, err = svc.GetServiceVersions(ctx, "unknown")
	require.Error(t, err)

	all := svc.ListVersionsForServices(ctx, []models.PlatformService{{Name: "trino"}, {Name: "gone"}})
	require.Equal(t, map[string][]string{"trino": {"481.0.0-1.0.0", "480.0.0-1.0.0"}}, all)
	require.Equal(t, map[string]string{"trino": "platform-charts", "webapp": "platform-charts"}, svc.CatalogOf(ctx))

	schema, err := svc.GetParameterSchema(ctx, "trino", "481.0.0-1.0.0")
	require.NoError(t, err)
	require.NotContains(t, schema["properties"], "global", "the platform fills global")
	require.Equal(t, []any{}, schema["required"])
	inputs, err := svc.GetPackageInputs(ctx, "trino", "481.0.0-1.0.0")
	require.NoError(t, err)
	require.Equal(t, []models.PackageInput{{Alias: "hive", Path: "hive", Parameter: "hive", Contract: "hive", Optional: true}}, inputs)

	schema, err = svc.GetParameterSchema(ctx, "trino", "")
	require.NoError(t, err)
	require.Equal(t, map[string]any{"type": "object"}, schema, "the default version has no config.schema.json: no parameter")

	_, err = svc.GetParameterSchema(ctx, "trino", "1.0.0")
	require.True(t, IsCatalogNotFound(err), "%v", err)
}
