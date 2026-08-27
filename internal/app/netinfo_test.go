package app

import (
	"net"
	"testing"
)

func TestIsPrivateLAN(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"10 block start", "10.0.0.1", true},
		{"10 block end", "10.255.255.255", true},
		{"172.16 block start", "172.16.0.1", true},
		{"172.16 block end", "172.31.255.255", true},
		{"172 below block", "172.15.255.255", false},
		{"172 above block", "172.32.0.0", false},
		{"192.168 block", "192.168.0.1", true},
		{"public address", "8.8.8.8", false},
		{"tailscale CGNAT is not LAN", "100.64.0.1", false},
		{"APIPA excluded", "169.254.1.1", false},
		{"ipv6 unsupported", "fe80::1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPrivateLAN(net.ParseIP(c.ip)); got != c.want {
				t.Errorf("isPrivateLAN(%q) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
	if isPrivateLAN(nil) {
		t.Error("isPrivateLAN(nil) = true, want false")
	}
}

func TestIsTailscaleCGNAT(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"block start", "100.64.0.1", true},
		{"mid block", "100.100.100.100", true},
		{"block end", "100.127.255.255", true},
		{"below block", "100.63.255.255", false},
		{"above block", "100.128.0.0", false},
		{"private LAN is not tailscale", "192.168.1.1", false},
		{"public address", "8.8.8.8", false},
		{"ipv6 unsupported", "fe80::1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTailscaleCGNAT(net.ParseIP(c.ip)); got != c.want {
				t.Errorf("isTailscaleCGNAT(%q) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
	if isTailscaleCGNAT(nil) {
		t.Error("isTailscaleCGNAT(nil) = true, want false")
	}
}
