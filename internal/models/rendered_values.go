package models

// RenderedValues are the values one upstream chart of an instance is
// rendered with: for a package instance, a dependency of its compiled
// umbrella, by key; for a plain chart, the chart itself. Next to the
// chart's own default values.
type RenderedValues struct {
	// Name is the chart key (the umbrella's dependency alias, else its name),
	// or the chart of a plain chart instance.
	Name string `json:"name" example:"trino"`
	// Chart is the chart that renders them (its name in its Chart.yaml).
	Chart string `json:"chart" example:"trino"`
	// ChartVersion is that chart, <name>-<version>.
	ChartVersion string `json:"chartVersion" example:"trino-1.42.2"`
	// ServiceVersion is the version of the instance's package (or plain
	// chart) the values were compiled for.
	ServiceVersion string `json:"serviceVersion" example:"480.0.0-1.0.2"`
	// Values is what Helm renders the chart with: its defaults coalesced
	// under the compiled values (instance.values.yaml of the compiled
	// directory, the key's; values.yaml for a plain chart). The compiled
	// values only when the defaults cannot be read (DefaultsError).
	Values string `json:"values"`
	// Defaults is the chart's own values.yaml, as published (with its
	// comments), from the chart cache. Empty when it could not be read (see
	// DefaultsError).
	Defaults string `json:"defaults,omitempty"`
	// ChangedLines are the 1-based lines of Values that differ from Defaults:
	// set by OKDP or the instance; the others come from the chart's
	// defaults. Empty when nothing differs or when the defaults could not be
	// read.
	ChangedLines []int `json:"changedLines"`
	// DefaultsError says why the defaults are missing.
	DefaultsError string `json:"defaultsError,omitempty"`
}
