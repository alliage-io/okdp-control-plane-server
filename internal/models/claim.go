package models

// ClaimKeys are the keys of a claim written in place of a connection's
// name, in an instance's parameters (a claim template): the class it is of,
// or the provider it names, and what it requests.
var ClaimKeys = []string{"class", "provider", "request"}

// NamedClaim is a named claim of a project: claims.<name> of its
// project.yaml, one connection of the contract for every instance of the
// project that names it, provisioned by the provider of its class.
type NamedClaim struct {
	// Name is the claim's name, a DNS-1123 label, no connection's name.
	Name string `json:"name"`
	// Contract is the contract of its connection (database-server, s3).
	Contract string `json:"contract"`
	// Class names a class of the contract (the project's or the
	// platform's); empty with Provider, or for the default class.
	Class string `json:"class,omitempty"`
	// Provider names the provider's instance, by its name or its release, of
	// the project or <namespace>/<instance>; never with Class.
	Provider string `json:"provider,omitempty"`
	// Request is what it requests of its provider, as the contract's claim
	// schema allows (x-okdp-claim).
	Request map[string]any `json:"request,omitempty"`
}

// NamedClaimView is a named claim as the API lists it: its declaration, the
// provider it is bound to, and the instances of the project that name it.
type NamedClaimView struct {
	NamedClaim
	// ID is the claim's ID, <project>.<name>: the name of the connection
	// its provider publishes for it.
	ID string `json:"id"`
	// Binding is where the compile of the project's sources binds it; nil
	// when it does not bind (Error says why) or no instance compiles.
	Binding *ClaimBinding `json:"binding,omitempty"`
	// Users are the instances of the project whose parameters name it.
	Users []string `json:"users"`
	// Error is why the claim does not bind, the compiler's message.
	Error string `json:"error,omitempty"`
}

// ClaimBinding is the provider a claim is bound to.
type ClaimBinding struct {
	// Namespace and Release are the provider's instance.
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	// Instance is the provider's instance name, in the project Namespace
	// (projects/<namespace>/instances/<instance>).
	Instance string `json:"instance,omitempty"`
	// Class is the class whose provider it is; empty when the claim names
	// its provider.
	Class string `json:"class,omitempty"`
	// Default reports that the class is the default one of the contract
	// (the claim names neither a class nor a provider).
	Default bool `json:"default"`
}

// NamedClaimRequest is the body of a named claim's creation or update.
type NamedClaimRequest struct {
	// Name is the claim's name, on creation (the path gives it on update).
	Name     string         `json:"name,omitempty"`
	Contract string         `json:"contract" binding:"required"`
	Class    string         `json:"class,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Request  map[string]any `json:"request,omitempty"`
}

// ProvisioningClass is a class of a contract's claims: the provider of the
// claims that name it, and what it fixes of their request.
type ProvisioningClass struct {
	Name string `json:"name"`
	// Provider is the provider's instance, <release> of the claimer's
	// project or <namespace>/<release>.
	Provider string `json:"provider"`
	// Request is what the class fixes of the request of its claims: a claim
	// that sets one of its keys is refused.
	Request map[string]any `json:"request,omitempty"`
	// Level is where the class is defined: platform (platform.yaml)
	// or project (the project's project.yaml).
	Level string `json:"level"`
}

// Levels of a class.
const (
	ClassLevelPlatform = "platform"
	ClassLevelProject  = "project"
)

// ContractClasses are the classes of a contract's claims for a project:
// the platform's and the project's (global.okdp.provisioning.<contract>).
type ContractClasses struct {
	Contract string `json:"contract"`
	// Default is the default class, of the claims that name neither a class
	// nor a provider; empty when there is none (such a claim is refused).
	Default string `json:"default,omitempty"`
	// DefaultLevel is where the default is set.
	DefaultLevel string              `json:"defaultLevel,omitempty"`
	Classes      []ProvisioningClass `json:"classes"`
	// ClaimSchema is what a claim may request (the contract's x-okdp-claim,
	// a JSON Schema); nil when the contract takes no request.
	ClaimSchema map[string]any `json:"claimSchema,omitempty"`
}

// ProvisioningResponse is the payload of GET
// /api/projects/:name/provisioning: the classes of each contract a claim
// may be of, in the project.
type ProvisioningResponse struct {
	Contracts []ContractClasses `json:"contracts"`
}

// ClaimTemplate is a claim written in place of a connection's name: a
// connection of the reference's own, provisioned by the provider of its
// class (or the one it names).
type ClaimTemplate struct {
	Class    string         `json:"class,omitempty"`
	Provider string         `json:"provider,omitempty"`
	Request  map[string]any `json:"request,omitempty"`
}

// BoundClaim is a claim of one of an instance's references, bound to its
// provider.
type BoundClaim struct {
	// Field is the parameter path of the reference (database,
	// catalogs[1].storage).
	Field    string `json:"field"`
	Contract string `json:"contract"`
	// ID is the claim's ID, the name of the connection its provider
	// publishes for it.
	ID string `json:"id"`
	// Name is the named claim the reference names; empty for a claim
	// written in the parameters (a claim template).
	Name    string       `json:"name,omitempty"`
	Binding ClaimBinding `json:"binding"`
	// Granted is the connection the provider publishes for the claim, when
	// it compiles.
	Granted *GrantedConnection `json:"granted,omitempty"`
}

// ProvidedClaim is a claim bound to a provider instance, and what it
// granted.
type ProvidedClaim struct {
	ID       string `json:"id"`
	Contract string `json:"contract"`
	// Namespace is the claimers' namespace.
	Namespace string `json:"namespace"`
	// Name is a named claim's name, empty for a claim template.
	Name string `json:"name,omitempty"`
	// Claimers are the instances whose references claim it: one for a claim
	// template, every instance that names a named claim.
	Claimers []ClaimUser `json:"claimers"`
	// Class is the class the claim binds through, empty when it names its
	// provider; Default reports the default class.
	Class   string `json:"class,omitempty"`
	Default bool   `json:"default"`
	// Granted is the connection the provider publishes for the claim (its
	// okdp.info.yaml), nil when it publishes none.
	Granted *GrantedConnection `json:"granted,omitempty"`
}

// ClaimUser is an instance whose reference claims a connection.
type ClaimUser struct {
	Namespace string `json:"namespace"`
	Instance  string `json:"instance"`
	Release   string `json:"release"`
	Field     string `json:"field"`
}

// GrantedConnection is what a provider granted a claim: the connection it
// publishes for it, its values completed against the contract (the size and
// the extensions granted, the bucket...), and its credentials Secret.
type GrantedConnection struct {
	Values    map[string]any `json:"values,omitempty"`
	SecretRef string         `json:"secretRef,omitempty"`
}

// InstanceClaims is the payload of GET
// /api/projects/:name/services/:serviceName/claims: the claims of the
// instance's references and where each is bound; when its package
// provisions contracts, the claims bound to it and what it granted.
type InstanceClaims struct {
	// Provisions are the contracts the instance's package provisions
	// (okdp.yaml provisions).
	Provisions []string `json:"provisions"`
	// Provided are the claims bound to the instance, as a provider.
	Provided []ProvidedClaim `json:"provided"`
	// Bound are the claims of the instance's references.
	Bound []BoundClaim `json:"bound"`
	// Error is why the instance does not compile, the compiler's message:
	// its claims are then unknown.
	Error string `json:"error,omitempty"`
}
