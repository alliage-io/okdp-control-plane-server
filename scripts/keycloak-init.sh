#!/bin/bash

# =============================================================================
# Keycloak Initialization Script
# Creates test users, roles, and OAuth2 clients for OKDP development
# =============================================================================

PUBLIC_CLIENT='public-oidc-client'
REDIRECT_URIS='[
  "http://localhost:4200/index.html",
  "http://localhost:4200/silent-refresh.html",
  "http://localhost:4200/home",
  "http://localhost:8093/swagger/oauth2-redirect.html"
]'
WEB_ORIGINS='["*"]'

echo "=== OKDP Keycloak Initialization ==="

# Connect to Keycloak
/opt/keycloak/bin/kcadm.sh config credentials \
    --server http://keycloak:$KC_HOSTNAME_PORT \
    --realm master \
    --user $KC_BOOTSTRAP_ADMIN_USERNAME \
    --password $KC_BOOTSTRAP_ADMIN_PASSWORD

# -----------------------------------------------------------------------------
# Create Users (password for all: "password")
# -----------------------------------------------------------------------------
echo "Creating users..."

# useradmin - Platform Administrator
/opt/keycloak/bin/kcadm.sh create users -r master \
    -s username=useradmin \
    -s firstName=User \
    -s lastName=Admin \
    -s enabled=true \
    -s email=useradmin@example.com \
    -s emailVerified=true
/opt/keycloak/bin/kcadm.sh set-password -r master --username useradmin --new-password password

# usera - Developer
/opt/keycloak/bin/kcadm.sh create users -r master \
    -s username=usera \
    -s firstName=User \
    -s lastName=A \
    -s enabled=true \
    -s email=usera@example.com \
    -s emailVerified=true
/opt/keycloak/bin/kcadm.sh set-password -r master --username usera --new-password password

# userb - Viewer
/opt/keycloak/bin/kcadm.sh create users -r master \
    -s username=userb \
    -s firstName=User \
    -s lastName=B \
    -s enabled=true \
    -s email=userb@example.com \
    -s emailVerified=true
/opt/keycloak/bin/kcadm.sh set-password -r master --username userb --new-password password

# -----------------------------------------------------------------------------
# Create Roles
# -----------------------------------------------------------------------------
echo "Creating roles..."
/opt/keycloak/bin/kcadm.sh create roles -r master -s name=admins
/opt/keycloak/bin/kcadm.sh create roles -r master -s name=developers
/opt/keycloak/bin/kcadm.sh create roles -r master -s name=viewers

# -----------------------------------------------------------------------------
# Assign Roles to Users
# -----------------------------------------------------------------------------
echo "Assigning roles..."
/opt/keycloak/bin/kcadm.sh add-roles -r master --uusername useradmin --rolename admins
/opt/keycloak/bin/kcadm.sh add-roles -r master --uusername usera --rolename developers
/opt/keycloak/bin/kcadm.sh add-roles -r master --uusername userb --rolename viewers

# -----------------------------------------------------------------------------
# Create OAuth2 Public Client
# -----------------------------------------------------------------------------
echo "Creating OAuth2 client..."
/opt/keycloak/bin/kcadm.sh create clients -r master \
    -s clientId=$PUBLIC_CLIENT \
    -s name=$PUBLIC_CLIENT \
    -s publicClient=true \
    -s directAccessGrantsEnabled=true \
    -s "redirectUris=${REDIRECT_URIS}" \
    -s "webOrigins=${WEB_ORIGINS}"

# -----------------------------------------------------------------------------
# Configure Realm
# -----------------------------------------------------------------------------
echo "Configuring realm..."
/opt/keycloak/bin/kcadm.sh update realms/master \
    -s accessTokenLifespan=28800 \
    -s sslRequired=NONE

echo ""
echo "=== Initialization Complete ==="
echo ""
echo "Test Users:"
echo "  useradmin / password  → Admin (can access /admin)"
echo "  usera     / password  → Developer"
echo "  userb     / password  → Viewer"
echo ""
echo "Keycloak Admin: http://localhost:7080 (admin/admin)"
echo ""
