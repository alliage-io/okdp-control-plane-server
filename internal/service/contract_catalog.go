package service

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"sync"
	"time"

	compiler "github.com/okdp/okdp-compiler"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// The contract schemas are those of the catalogs (contracts/ of each
// repository of catalogs.yaml), read as the compile reads them
// (compiler.ReadContracts). The server maps each to the form the console
// renders (a ContractDescriptor) and to the address shown for a connection
// of that contract; a connection is checked by the compiler itself
// (compiler.CheckConnection), never here.

// ContractCatalog exposes the known contracts.
type ContractCatalog interface {
	List() []models.ContractDescriptor
	Get(name string) (*models.ContractDescriptor, bool)
	// Normalize fills the derived fields and drops the ones the submitted
	// values put out of play, so that the check and the storage see a
	// coherent set. Called before the check, on both create and update.
	Normalize(typeName string, values map[string]any) map[string]any
	// Contracts are the contracts as the compile reads them, for
	// compiler.CheckConnection.
	Contracts() fs.FS
}

type embeddedCatalog struct {
	fsys   fs.FS
	types  []models.ContractDescriptor
	byName map[string]*models.ContractDescriptor
}

// NewContractCatalog loads the contracts of a directory of contract
// schemas (<contract>.schema.json), as the compile reads them.
func NewContractCatalog(contractsFS fs.FS) (ContractCatalog, error) {
	contracts, err := compiler.ReadContracts(contractsFS)
	if err != nil {
		return nil, fmt.Errorf("failed to read the contracts: %w", err)
	}
	c := &embeddedCatalog{fsys: contractsFS, byName: map[string]*models.ContractDescriptor{}}
	for i := range contracts {
		c.types = append(c.types, formOf(&contracts[i]))
	}
	sort.Slice(c.types, func(i, j int) bool { return c.types[i].Name < c.types[j].Name })
	for i := range c.types {
		c.byName[c.types[i].Name] = &c.types[i]
	}
	return c, nil
}

// formOf maps a contract, as the compile reads it, to the form the console
// renders (GET /api/contracts): its fields in the order of the file, each
// with the hints of its property (title, type, enum, bounds, x-ui-*), a
// secret one required when its x-okdp-required says so. A field of a list
// (trino catalogs) is published by charts, never typed into a form: not
// offered.
func formOf(d *compiler.ContractDescriptor) models.ContractDescriptor {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	ct := models.ContractDescriptor{
		Name:        d.Name,
		DisplayName: d.Title,
		Description: d.Description,
		Icon:        str(d.Schema, "x-okdp-icon"),
		Category:    str(d.Schema, "x-okdp-category"),
		ClaimSchema: d.Claim,
	}
	ct.External, _ = d.Schema["x-okdp-external"].(bool)
	if from, ok := d.Schema["x-okdp-endpoint-from"].([]any); ok {
		for _, f := range from {
			if s, ok := f.(string); ok {
				ct.EndpointFrom = append(ct.EndpointFrom, s)
			}
		}
	}
	required := map[string]bool{}
	if list, ok := d.Schema["required"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	for _, f := range d.Fields {
		p := f.Property
		secretRequired, _ := p["x-okdp-required"].(bool)
		field := models.ConnectionField{
			Name:        f.Name,
			Label:       str(p, "title"),
			Required:    required[f.Name] || (f.Secret && secretRequired),
			Secret:      f.Secret,
			Masked:      str(p, "x-ui-widget") == "password",
			Default:     f.Default,
			Placeholder: str(p, "x-ui-placeholder"),
			Help:        str(p, "description"),
			Min:         number(p["minimum"]),
			Max:         number(p["maximum"]),
		}
		if f.Condition != nil {
			field.ShowWhen = &models.FieldCondition{Field: str(f.Condition, "field"), Value: fmt.Sprint(f.Condition["value"])}
		}
		if f.Derived != nil {
			derived := &models.FieldDerivation{From: str(f.Derived, "from"), Map: map[string]string{}}
			if m, ok := f.Derived["map"].(map[string]any); ok {
				for k, v := range m {
					derived.Map[k] = fmt.Sprint(v)
				}
			}
			field.Derived = derived
		}
		enum, _ := p["enum"].([]any)
		switch kind := str(p, "type"); {
		case len(enum) > 0:
			field.Type = models.FieldTypeEnum
			for _, option := range enum {
				field.Options = append(field.Options, fmt.Sprint(option))
			}
		case kind == "integer" || kind == "number":
			field.Type = models.FieldTypeNumber
		case kind == "boolean":
			field.Type = models.FieldTypeBoolean
		case kind == "string":
			field.Type = models.FieldTypeString
		default:
			continue
		}
		ct.Fields = append(ct.Fields, field)
	}
	return ct
}

// number is a bound of a property, nil when it has none.
func number(v any) *float64 {
	if n, ok := toFloat(v); ok {
		return &n
	}
	return nil
}

func (c *embeddedCatalog) List() []models.ContractDescriptor {
	out := make([]models.ContractDescriptor, len(c.types))
	copy(out, c.types)
	return out
}

func (c *embeddedCatalog) Get(name string) (*models.ContractDescriptor, bool) {
	ct, ok := c.byName[name]
	return ct, ok
}

func (c *embeddedCatalog) Contracts() fs.FS { return c.fsys }

// Normalize returns a copy of values with the derived fields computed and the
// fields whose condition is not met removed. A MySQL connection carrying a
// PostgreSQL sslMode, or a driver contradicting its engine, cannot be opened by
// anything. Neither should be storable.
func (c *embeddedCatalog) Normalize(typeName string, values map[string]any) map[string]any {
	ct, ok := c.Get(typeName)
	if !ok {
		return values
	}

	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}

	for i := range ct.Fields {
		f := &ct.Fields[i]
		if !f.Applies(out) {
			delete(out, f.Name)
			continue
		}
		if f.Derived != nil {
			source, _ := out[f.Derived.From].(string)
			if derived, known := f.Derived.Map[source]; known {
				out[f.Name] = derived
			}
			continue
		}
		// An omitted field means the contract's default, and it belongs in the
		// values: the connectivity test and the consumers read those, not the
		// descriptor, so an absent sslMode would be probed as the strictest mode
		// rather than the declared one.
		if _, present := out[f.Name]; !present && f.Default != nil && !f.Secret {
			out[f.Name] = f.Default
		}
	}
	return out
}

func toFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// ContractsSource returns the contract schemas of the platform: the union
// of the catalogs' contracts/.
type ContractsSource func(ctx context.Context) (fs.FS, error)

// contractsRefresh is how stale the contracts of the catalogs may be.
const contractsRefresh = 30 * time.Second

// catalogContracts is the ContractCatalog of the catalogs' contracts, read
// again every contractsRefresh. A refresh that fails keeps the last
// contracts read, with a warning; none read yet is an empty catalog.
type catalogContracts struct {
	source ContractsSource

	mu       sync.Mutex
	current  ContractCatalog
	loadedAt time.Time
}

// NewCatalogContracts serves the contracts source gives.
func NewCatalogContracts(source ContractsSource) ContractCatalog {
	return &catalogContracts{source: source}
}

func (c *catalogContracts) get() ContractCatalog {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil && time.Since(c.loadedAt) < contractsRefresh {
		return c.current
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	loaded, err := func() (ContractCatalog, error) {
		fsys, err := c.source(ctx)
		if err != nil {
			return nil, err
		}
		return NewContractCatalog(fsys)
	}()
	if err != nil {
		slog.Warn("Could not read the contracts of the catalogs", "error", err)
		if c.current == nil {
			empty, _ := NewContractCatalog(emptyFS{})
			return empty
		}
		return c.current
	}
	c.current, c.loadedAt = loaded, time.Now()
	return loaded
}

func (c *catalogContracts) List() []models.ContractDescriptor { return c.get().List() }
func (c *catalogContracts) Get(name string) (*models.ContractDescriptor, bool) {
	return c.get().Get(name)
}
func (c *catalogContracts) Normalize(typeName string, values map[string]any) map[string]any {
	return c.get().Normalize(typeName, values)
}
func (c *catalogContracts) Contracts() fs.FS { return c.get().Contracts() }

// emptyFS is a directory with nothing in it.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (emptyFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, nil }
