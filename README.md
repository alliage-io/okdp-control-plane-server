# okdp-control-plane-server

Minimal Go server for OKDP UI New, featuring a standard layered architecture and Kubernetes integration.

## Prerequisites

- **Go**: 1.24+
- **Kubernetes Cluster**: Required for Project features (local or remote).

## Setup

Projects are plain Kubernetes Namespaces labeled `okdp.io/project=<name>` —
no custom CRD or operator is required.

1. **Run Server**
   ```bash
   go run cmd/server/main.go
   ```
   The server starts on port `8093`.

## API Documentation

Swagger UI is available at:
http://localhost:8093/swagger/index.html

## Project Structure

- `cmd/server`: Entry point.
- `internal/api`: HTTP handlers and router.
- `internal/models`: Domain models.
- `internal/repository`: Data access (K8s client).
- `internal/service`: Business logic.

## Identity Backends

User and group management (`/api/v1/identity`) supports two backends, selected
with the `IDENTITY_BACKEND` environment variable:

- **`keycloak`** (default): users and groups are managed through the Keycloak
  Admin REST API. Users map to Keycloak users; groups map to Keycloak **realm
  roles** (the OKDP sandbox maps realm roles to the `groups` token claim). Used
  by the [okdp-sandbox](https://github.com/OKDP/okdp-sandbox), which ships
  Keycloak as its identity provider.
- **`kubauth`**: users, groups, and group bindings are stored as
  [kubauth](https://github.com/kubotal/kubauth) CRDs (`kubauth.kubotal.io/v1alpha1`)
  in `PLATFORM_NAMESPACE`.

Keycloak backend configuration:

| Variable | Default | Description |
|----------|---------|-------------|
| `IDENTITY_BACKEND` | `keycloak` | `keycloak` or `kubauth` |
| `KEYCLOAK_URL` | `http://localhost:7080` | Keycloak base URL |
| `KEYCLOAK_REALM` | `master` | Realm to authenticate against and manage |
| `KEYCLOAK_CLIENT_ID` | `admin-cli` | Client used to obtain the admin token |
| `KEYCLOAK_CLIENT_SECRET` | _(empty)_ | If set, uses the `client_credentials` grant |
| `KEYCLOAK_ADMIN_USER` | `admin` | Admin user for the `password` grant (when no client secret) |
| `KEYCLOAK_ADMIN_PASSWORD` | `admin` | Admin password for the `password` grant |
| `KEYCLOAK_TLS_INSECURE` | `false` | Skip TLS verification (self-signed sandbox certificates) |

## Dev Setup (Keycloak + Server)

```bash
# 1. Start Keycloak
docker compose up -d

# 2. Run server (Keycloak-managed identities by default)
go run cmd/server/main.go
```

**Test Users** (password: `password`):
| User      | Role       | Access           |
|-----------|------------|------------------|
| useradmin | admins     | Admin space      |
| usera     | developers | Project space    |
| userb     | viewers    | Project space    |

Keycloak Admin: http://localhost:7080 (`admin` / `admin`)
