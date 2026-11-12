// Package provisioning names the platform's OIDC client provisioning
// backend (global.okdp.identity.provisioning.provider), reported to the
// console by the capabilities.
//
// The control plane provisions no OAuth client and deletes none: in
// global.okdp.oidc.clientProvisioning dcr mode, the oidc-dcr Jobs of the
// compiled output register each instance's clients; in existing mode, the
// platform provisions them beforehand. Deleting an instance never touches
// Keycloak (a client named after its release could be anyone's).
package provisioning

// Provider names accepted in the platform values (global.okdp.identity.provisioning.provider).
const (
	ProviderNone     = "none"
	ProviderKeycloak = "keycloak"
)
