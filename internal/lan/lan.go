// Package lan resolves the machine's LAN address so the dashboard can be
// reached from another device (e.g. a phone) on the same network without the
// user typing or knowing their IP — used by `serve --lan` to print the URL and
// by `serve --lan --qr` to build the QR target (architecture.md §4.3).
package lan

import "net"

// IPv4 returns the machine's first non-loopback IPv4 address, or "" if none is
// found (e.g. no network, only IPv6). Callers should treat "" as "LAN access
// not available right now" and fall back to localhost.
func IPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}
