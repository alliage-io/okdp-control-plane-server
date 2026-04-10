package models

// ImportClusterRequest represents a request to import a cluster via kubeconfig
type ImportClusterRequest struct {
	Name              string   `json:"name"`
	Kubeconfig        string   `json:"kubeconfig"`        // Base64 encoded content
	AllowedNamespaces []string `json:"allowedNamespaces"` // Optional
	Capabilities      []string `json:"capabilities"`      // Optional
}
