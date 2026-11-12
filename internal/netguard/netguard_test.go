package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestTheGuardRefusesThePodsOwnNetworkPosition(t *testing.T) {
	g, err := New(nil, []string{"10.96.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{
		"127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1",
		"169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1",
		"10.96.0.1",
	} {
		if err := g.Check(netip.MustParseAddr(addr)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: want refused, got %v", addr, err)
		}
	}
	for _, addr := range []string{"10.0.0.7", "192.168.1.10", "8.8.8.8", "2001:db8::1"} {
		if err := g.Check(netip.MustParseAddr(addr)); err != nil {
			t.Errorf("%s: want allowed, got %v", addr, err)
		}
	}
}

func TestTheAllowListOpensARefusedNetwork(t *testing.T) {
	g, err := New([]string{"127.0.0.0/8", "169.254.10.10"}, []string{"10.96.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1", "169.254.10.10"} {
		if err := g.Check(netip.MustParseAddr(addr)); err != nil {
			t.Errorf("%s: allow-listed, got %v", addr, err)
		}
	}
	if err := g.Check(netip.MustParseAddr("169.254.169.254")); !errors.Is(err, ErrRefused) {
		t.Errorf("the metadata endpoint is not allow-listed, got %v", err)
	}
	if _, err := New([]string{"not-a-cidr"}, nil); err == nil {
		t.Error("an invalid allow-list entry must be refused")
	}
}

// The dial itself is refused, after resolution: a listener on the loopback
// never sees the connection.
func TestTheDialerNeverReachesALoopbackListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()

	_, err = Default().Dialer(time.Second).DialContext(context.Background(), "tcp", ln.Addr().String())
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("want ErrRefused, got %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_, err = Default().DialContext(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("a name resolving to the loopback: want ErrRefused, got %v", err)
	}
	select {
	case <-accepted:
		t.Fatal("the listener accepted a connection")
	case <-time.After(100 * time.Millisecond):
	}
}
