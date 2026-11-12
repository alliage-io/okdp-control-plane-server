// Package netguard refuses the addresses a connection made on a caller's
// behalf must never reach: the pod itself (loopback), the cloud metadata
// endpoints and the rest of link-local, the unspecified and multicast
// addresses, and the Kubernetes API server. It checks the address the
// socket connects to, after name resolution, so a name that resolves to a
// refused address (DNS rebinding included) is refused too.
//
// The SQL editor and the connection tests dial through it: a host:port taken
// from a connection, whoever declared it, is never a way into the pod's own
// network position.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"
)

// ErrRefused is the error of a dial to a refused address.
var ErrRefused = errors.New("the address is not reachable from the control plane")

// Guard decides which addresses a dial may reach.
type Guard struct {
	// allowed are the networks dialled even when they fall in a refused
	// range (the platform's allow-list).
	allowed []netip.Prefix
	// apiServer are the addresses of the Kubernetes API server.
	apiServer []netip.Addr
}

// New returns a Guard that refuses loopback, link-local, unspecified and
// multicast addresses and the API server's, except in the allowed
// networks (CIDRs, or single addresses). apiServer lists the API server's
// addresses; KubernetesAPIServer gives the in-cluster one.
func New(allowed []string, apiServer []string) (*Guard, error) {
	g := &Guard{}
	for _, raw := range allowed {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := parsePrefix(raw)
		if err != nil {
			return nil, fmt.Errorf("allowed network %q: %w", raw, err)
		}
		g.allowed = append(g.allowed, prefix)
	}
	for _, raw := range apiServer {
		if addr, err := netip.ParseAddr(strings.TrimSpace(raw)); err == nil {
			g.apiServer = append(g.apiServer, addr.Unmap())
		}
	}
	return g, nil
}

// Default is the Guard of no allow-list, the in-cluster API server
// refused.
func Default() *Guard {
	g, _ := New(nil, KubernetesAPIServer())
	return g
}

// KubernetesAPIServer returns the address of the API server's Service in
// a pod (KUBERNETES_SERVICE_HOST), none outside a cluster.
func KubernetesAPIServer() []string {
	if host := os.Getenv("KUBERNETES_SERVICE_HOST"); host != "" {
		return []string{host}
	}
	return nil
}

func parsePrefix(raw string) (netip.Prefix, error) {
	if strings.Contains(raw, "/") {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Check returns ErrRefused, wrapped, when addr may not be dialled.
func (g *Guard) Check(addr netip.Addr) error {
	addr = addr.Unmap()
	for _, p := range g.allowed {
		if p.Contains(addr) {
			return nil
		}
	}
	reason := ""
	switch {
	case addr.IsLoopback():
		reason = "loopback"
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		reason = "link-local"
	case addr.IsUnspecified():
		reason = "unspecified"
	case addr.IsMulticast(), addr.IsInterfaceLocalMulticast():
		reason = "multicast"
	}
	for _, a := range g.apiServer {
		if a == addr {
			reason = "the Kubernetes API server"
		}
	}
	if reason != "" {
		return fmt.Errorf("%w: %s is %s", ErrRefused, addr, reason)
	}
	return nil
}

// Control is a net.Dialer Control function: it refuses the connection to
// a refused address, the address the socket is about to connect to.
func (g *Guard) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrRefused, host)
	}
	return g.Check(addr)
}

// Dialer returns a net.Dialer that dials through the guard.
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: g.Control}
}

// DialContext dials through the guard.
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return g.Dialer(30*time.Second).DialContext(ctx, network, address)
}
