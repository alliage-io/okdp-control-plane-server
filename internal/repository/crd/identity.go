package crd

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// User represents the kubauth User CRD
type User struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UserSpec `json:"spec,omitempty"`
}

type UserSpec struct {
	Name         string                `json:"name,omitempty"`
	Emails       []string              `json:"emails,omitempty"`
	PasswordHash string                `json:"passwordHash,omitempty"`
	Uid          *int                  `json:"uid,omitempty"`
	Comment      string                `json:"comment,omitempty"`
	Claims       *apiextensionsv1.JSON `json:"claims,omitempty"`
	Disabled     *bool                 `json:"disabled,omitempty"`

	// Password carries the plaintext password to identity backends that
	// manage credentials themselves (e.g. Keycloak). Excluded from JSON so
	// it is never persisted in the kubauth User CRD.
	Password string `json:"-"`
}

type UserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []User `json:"items"`
}

// Group represents the kubauth Group CRD
type Group struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GroupSpec `json:"spec,omitempty"`
}

type GroupSpec struct {
	Comment string                `json:"comment,omitempty"`
	Claims  *apiextensionsv1.JSON `json:"claims,omitempty"`
}

type GroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Group `json:"items"`
}

// GroupBinding represents the kubauth GroupBinding CRD
type GroupBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GroupBindingSpec `json:"spec,omitempty"`
}

type GroupBindingSpec struct {
	User  string `json:"user"`
	Group string `json:"group"`
}

type GroupBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GroupBinding `json:"items"`
}
