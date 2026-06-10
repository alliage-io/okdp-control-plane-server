package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okdp/okdp-server-new/internal/config"
	"github.com/okdp/okdp-server-new/internal/models"
	"github.com/okdp/okdp-server-new/internal/repository/crd"
	"github.com/sirupsen/logrus"
)

// keycloakIdentityRepository implements IdentityRepository against the
// Keycloak Admin REST API. Users map to Keycloak users; Groups map to
// Keycloak realm roles (the platform maps realm roles to the `groups`
// token claim); GroupBindings map to user realm-role assignments.
type keycloakIdentityRepository struct {
	baseURL       string
	realm         string
	clientID      string
	clientSecret  string
	adminUser     string
	adminPassword string

	httpClient *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// Realm roles managed by Keycloak itself, hidden from the identity API.
func isBuiltInRole(name string) bool {
	return name == "offline_access" ||
		name == "uma_authorization" ||
		strings.HasPrefix(name, "default-roles-")
}

func NewKeycloakIdentityRepository(cfg *config.Config) IdentityRepository {
	transport := http.DefaultTransport
	if cfg.KeycloakTLSInsecure {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	return &keycloakIdentityRepository{
		baseURL:       strings.TrimRight(cfg.KeycloakURL, "/"),
		realm:         cfg.KeycloakRealm,
		clientID:      cfg.KeycloakClientID,
		clientSecret:  cfg.KeycloakClientSecret,
		adminUser:     cfg.KeycloakAdminUser,
		adminPassword: cfg.KeycloakAdminPassword,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
	}
}

// --- Keycloak API representations ---

type kcUser struct {
	ID         string              `json:"id,omitempty"`
	Username   string              `json:"username"`
	FirstName  string              `json:"firstName,omitempty"`
	LastName   string              `json:"lastName,omitempty"`
	Email      string              `json:"email,omitempty"`
	Enabled    bool                `json:"enabled"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type kcRole struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type kcCredential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// --- Token management ---

func (r *keycloakIdentityRepository) getToken(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.token != "" && time.Now().Before(r.tokenExpiry) {
		return r.token, nil
	}

	form := url.Values{}
	form.Set("client_id", r.clientID)
	if r.clientSecret != "" {
		form.Set("grant_type", "client_credentials")
		form.Set("client_secret", r.clientSecret)
	} else {
		form.Set("grant_type", "password")
		form.Set("username", r.adminUser)
		form.Set("password", r.adminPassword)
	}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", r.baseURL, r.realm)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak token request failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to decode keycloak token response: %w", err)
	}

	r.token = tokenResp.AccessToken
	// Refresh slightly before expiry
	r.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn-30) * time.Second)
	return r.token, nil
}

// doRequest performs an authenticated request against the admin API.
// path is relative to /admin/realms/{realm}. out may be nil.
func (r *keycloakIdentityRepository) doRequest(ctx context.Context, method, path string, payload, out interface{}) error {
	token, err := r.getToken(ctx)
	if err != nil {
		return err
	}

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	reqURL := fmt.Sprintf("%s/admin/realms/%s%s", r.baseURL, r.realm, path)
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("keycloak request %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("keycloak request %s %s failed (%d): %s", method, path, resp.StatusCode, string(respBody))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("failed to decode keycloak response for %s %s: %w", method, path, err)
		}
	}
	return nil
}

// --- Mapping helpers ---

func attr(attrs map[string][]string, key string) string {
	if vals, ok := attrs[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func toModelUser(u *kcUser) models.User {
	displayName := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))

	var emails []string
	if u.Email != "" {
		emails = []string{u.Email}
	}

	uid := 0
	if rawUID := attr(u.Attributes, "uid"); rawUID != "" {
		if parsed, err := strconv.Atoi(rawUID); err == nil {
			uid = parsed
		}
	}

	return models.User{
		Username: u.Username,
		Name:     displayName,
		Email:    emails,
		Comment:  attr(u.Attributes, "comment"),
		UID:      uid,
		Disabled: !u.Enabled,
	}
}

func toKcUser(user *crd.User) *kcUser {
	// The display name is stored in firstName/lastName, split on the last space.
	firstName := strings.TrimSpace(user.Spec.Name)
	lastName := ""
	if idx := strings.LastIndex(firstName, " "); idx > 0 {
		lastName = firstName[idx+1:]
		firstName = firstName[:idx]
	}

	email := ""
	if len(user.Spec.Emails) > 0 {
		email = user.Spec.Emails[0]
	}

	enabled := true
	if user.Spec.Disabled != nil {
		enabled = !*user.Spec.Disabled
	}

	attrs := map[string][]string{}
	if user.Spec.Comment != "" {
		attrs["comment"] = []string{user.Spec.Comment}
	}
	if user.Spec.Uid != nil {
		attrs["uid"] = []string{strconv.Itoa(*user.Spec.Uid)}
	}

	username := user.ObjectMeta.Name
	if username == "" {
		username = user.Spec.Name
	}

	return &kcUser{
		Username:   username,
		FirstName:  firstName,
		LastName:   lastName,
		Email:      email,
		Enabled:    enabled,
		Attributes: attrs,
	}
}

func (r *keycloakIdentityRepository) findUser(ctx context.Context, username string) (*kcUser, error) {
	var users []kcUser
	path := fmt.Sprintf("/users?username=%s&exact=true", url.QueryEscape(username))
	if err := r.doRequest(ctx, http.MethodGet, path, nil, &users); err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].Username, username) {
			return &users[i], nil
		}
	}
	return nil, fmt.Errorf("user %s not found in keycloak realm %s", username, r.realm)
}

func (r *keycloakIdentityRepository) setPassword(ctx context.Context, userID, password string) error {
	cred := kcCredential{Type: "password", Value: password, Temporary: false}
	return r.doRequest(ctx, http.MethodPut, fmt.Sprintf("/users/%s/reset-password", userID), cred, nil)
}

// --- Users ---

func (r *keycloakIdentityRepository) ListUsers(ctx context.Context) ([]models.User, error) {
	var kcUsers []kcUser
	if err := r.doRequest(ctx, http.MethodGet, "/users?max=1000", nil, &kcUsers); err != nil {
		return nil, err
	}

	var users []models.User
	for i := range kcUsers {
		users = append(users, toModelUser(&kcUsers[i]))
	}
	return users, nil
}

func (r *keycloakIdentityRepository) GetUser(ctx context.Context, name string) (*models.User, error) {
	kcU, err := r.findUser(ctx, name)
	if err != nil {
		return nil, err
	}
	user := toModelUser(kcU)
	return &user, nil
}

func (r *keycloakIdentityRepository) CreateUser(ctx context.Context, user *crd.User) error {
	kcU := toKcUser(user)
	if err := r.doRequest(ctx, http.MethodPost, "/users", kcU, nil); err != nil {
		return err
	}

	if user.Spec.Password != "" {
		created, err := r.findUser(ctx, kcU.Username)
		if err != nil {
			return fmt.Errorf("user created but lookup for password setup failed: %w", err)
		}
		if err := r.setPassword(ctx, created.ID, user.Spec.Password); err != nil {
			return fmt.Errorf("user created but failed to set password: %w", err)
		}
	}
	return nil
}

func (r *keycloakIdentityRepository) UpdateUser(ctx context.Context, user *crd.User) error {
	username := user.ObjectMeta.Name
	if username == "" {
		username = user.Spec.Name
	}

	existing, err := r.findUser(ctx, username)
	if err != nil {
		return err
	}

	kcU := toKcUser(user)
	kcU.ID = existing.ID
	if err := r.doRequest(ctx, http.MethodPut, "/users/"+existing.ID, kcU, nil); err != nil {
		return err
	}

	if user.Spec.Password != "" {
		if err := r.setPassword(ctx, existing.ID, user.Spec.Password); err != nil {
			return fmt.Errorf("user updated but failed to set password: %w", err)
		}
	}
	return nil
}

func (r *keycloakIdentityRepository) DeleteUser(ctx context.Context, name string) error {
	existing, err := r.findUser(ctx, name)
	if err != nil {
		return err
	}
	return r.doRequest(ctx, http.MethodDelete, "/users/"+existing.ID, nil, nil)
}

// --- Groups (Keycloak realm roles) ---

func (r *keycloakIdentityRepository) ListGroups(ctx context.Context) ([]models.Group, error) {
	var roles []kcRole
	if err := r.doRequest(ctx, http.MethodGet, "/roles?max=1000", nil, &roles); err != nil {
		return nil, err
	}

	var groups []models.Group
	for _, role := range roles {
		if isBuiltInRole(role.Name) {
			continue
		}
		groups = append(groups, models.Group{
			Name:        role.Name,
			Comment:     role.Description,
			Description: role.Description,
		})
	}
	return groups, nil
}

func (r *keycloakIdentityRepository) GetGroup(ctx context.Context, name string) (*models.Group, error) {
	var role kcRole
	if err := r.doRequest(ctx, http.MethodGet, "/roles/"+url.PathEscape(name), nil, &role); err != nil {
		return nil, err
	}
	return &models.Group{
		Name:        role.Name,
		Comment:     role.Description,
		Description: role.Description,
	}, nil
}

func (r *keycloakIdentityRepository) CreateGroup(ctx context.Context, group *crd.Group) error {
	role := kcRole{
		Name:        group.ObjectMeta.Name,
		Description: group.Spec.Comment,
	}
	return r.doRequest(ctx, http.MethodPost, "/roles", role, nil)
}

func (r *keycloakIdentityRepository) UpdateGroup(ctx context.Context, group *crd.Group) error {
	role := kcRole{
		Name:        group.ObjectMeta.Name,
		Description: group.Spec.Comment,
	}
	return r.doRequest(ctx, http.MethodPut, "/roles/"+url.PathEscape(role.Name), role, nil)
}

func (r *keycloakIdentityRepository) DeleteGroup(ctx context.Context, name string) error {
	return r.doRequest(ctx, http.MethodDelete, "/roles/"+url.PathEscape(name), nil, nil)
}

// --- GroupBindings (user realm-role assignments) ---

func (r *keycloakIdentityRepository) userRoles(ctx context.Context, userID string) ([]kcRole, error) {
	var roles []kcRole
	if err := r.doRequest(ctx, http.MethodGet, fmt.Sprintf("/users/%s/role-mappings/realm", userID), nil, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

func (r *keycloakIdentityRepository) ListGroupBindings(ctx context.Context, userFilter string) ([]models.GroupBinding, error) {
	var kcUsers []kcUser
	if userFilter != "" {
		u, err := r.findUser(ctx, userFilter)
		if err != nil {
			return nil, err
		}
		kcUsers = []kcUser{*u}
	} else {
		if err := r.doRequest(ctx, http.MethodGet, "/users?max=1000", nil, &kcUsers); err != nil {
			return nil, err
		}
	}

	var bindings []models.GroupBinding
	for i := range kcUsers {
		roles, err := r.userRoles(ctx, kcUsers[i].ID)
		if err != nil {
			logrus.WithError(err).WithField("user", kcUsers[i].Username).Warn("Failed to list keycloak role mappings")
			continue
		}
		for _, role := range roles {
			if isBuiltInRole(role.Name) {
				continue
			}
			bindings = append(bindings, models.GroupBinding{
				User:  kcUsers[i].Username,
				Group: role.Name,
			})
		}
	}
	return bindings, nil
}

func (r *keycloakIdentityRepository) CreateGroupBinding(ctx context.Context, binding *crd.GroupBinding) error {
	user, err := r.findUser(ctx, binding.Spec.User)
	if err != nil {
		return err
	}

	var role kcRole
	if err := r.doRequest(ctx, http.MethodGet, "/roles/"+url.PathEscape(binding.Spec.Group), nil, &role); err != nil {
		return err
	}

	return r.doRequest(ctx, http.MethodPost, fmt.Sprintf("/users/%s/role-mappings/realm", user.ID), []kcRole{role}, nil)
}

func (r *keycloakIdentityRepository) DeleteGroupBinding(ctx context.Context, name string) error {
	// Bindings have no standalone identity in Keycloak; callers must use
	// DeleteGroupBindingByRef (user + group).
	return fmt.Errorf("deleting a group binding by name is not supported by the keycloak identity backend")
}

func (r *keycloakIdentityRepository) DeleteGroupBindingByRef(ctx context.Context, user, group string) error {
	kcU, err := r.findUser(ctx, user)
	if err != nil {
		return err
	}

	var role kcRole
	if err := r.doRequest(ctx, http.MethodGet, "/roles/"+url.PathEscape(group), nil, &role); err != nil {
		return err
	}

	return r.doRequest(ctx, http.MethodDelete, fmt.Sprintf("/users/%s/role-mappings/realm", kcU.ID), []kcRole{role}, nil)
}
