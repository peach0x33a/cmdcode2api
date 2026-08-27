package app

import "net"

var (
	lanBlock10     = mustCIDR("10.0.0.0/8")
	lanBlock172    = mustCIDR("172.16.0.0/12")
	lanBlock192    = mustCIDR("192.168.0.0/16")
	tailscaleBlock = mustCIDR("100.64.0.0/10")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isPrivateLAN reports whether ip is an RFC1918 private IPv4 address
// (10/8, 172.16/12, 192.168/16). It deliberately excludes the 169.254/16
// APIPA range (a DHCP-failure address, not a usable LAN URL) and any IPv6
// or nil input.
func isPrivateLAN(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return lanBlock10.Contains(v4) || lanBlock172.Contains(v4) || lanBlock192.Contains(v4)
}

// isTailscaleCGNAT reports whether ip falls in Tailscale's fixed IPv4
// CGNAT range (100.64.0.0/10), which it assigns to every node in a
// tailnet. This range isn't used by other common consumer/enterprise
// network gear, so it reliably identifies a Tailscale-assigned address
// without needing the tailscale CLI.
func isTailscaleCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return tailscaleBlock.Contains(v4)
}

// detectLANIPv4 finds this machine's LAN-facing IPv4 address by asking the
// OS routing table which local address it would use to reach the public
// internet — via a UDP "connect" that never actually sends a packet. This
// is more reliable than enumerating net.Interfaces() and guessing, since
// virtual adapters (WSL2, Hyper-V, Docker Desktop, VMware, ...) also hand
// out RFC1918 addresses and there's no dependable way to tell those apart
// from the real LAN NIC by inspection alone.
func detectLANIPv4() (net.IP, bool) {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return nil, false
	}
	if !isPrivateLAN(addr.IP) {
		return nil, false
	}
	return addr.IP, true
}

// detectTailscaleIPv4 scans this machine's network interfaces for an
// address in Tailscale's CGNAT range, returning the first match. It skips
// interfaces that are down and tolerates a per-interface Addrs() error
// (some virtual adapters report one on Windows) by skipping just that
// interface rather than aborting the whole scan.
func detectTailscaleIPv4() (net.IP, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if isTailscaleCGNAT(ipNet.IP) {
				return ipNet.IP.To4(), true
			}
		}
	}
	return nil, false
}
