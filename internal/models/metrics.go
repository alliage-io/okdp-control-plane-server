package models

// ServiceMetrics aggregates the live resource usage of all pods belonging to a
// service instance. Values come from the Kubernetes metrics.k8s.io API; limits
// are read from each pod's container spec.
type ServiceMetrics struct {
	CPU    MetricValue `json:"cpu"`
	Memory MetricValue `json:"memory"`
}

// MetricValue holds both raw (normalized) and human-readable resource figures.
//
// CPU is expressed in cores (e.g. 1.5 = 1500 millicores). Memory is in bytes.
// The human-readable fields follow Kubernetes conventions: "1.5" CPU, "4Gi" memory.
type MetricValue struct {
	UsedRaw   float64 `json:"usedRaw"`
	LimitRaw  float64 `json:"limitRaw"`
	Used      string  `json:"used"`
	Limit     string  `json:"limit"`
	Pct       float64 `json:"pct"`
	Available bool    `json:"available"`
}
