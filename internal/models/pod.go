package models

// Pod represents a Kubernetes pod belonging to a service instance.
type Pod struct {
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Ready      string      `json:"ready"`
	Restarts   int32       `json:"restarts"`
	Age        string      `json:"age"`
	Containers []Container `json:"containers"`
}

// Container represents a container within a pod (infra sidecars are filtered out).
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Ready bool   `json:"ready"`
}
