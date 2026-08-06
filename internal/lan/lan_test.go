package lan

import (
	"net"
	"testing"
)

// IPv4 is environment-dependent, so we only assert its contract: it returns
// either "" or a syntactically valid, non-loopback IPv4 string.
func TestIPv4_ContractOnly(t *testing.T) {
	got := IPv4()
	if got == "" {
		return
	}
	ip := net.ParseIP(got)
	if ip == nil {
		t.Fatalf("IPv4() = %q, not a valid IP", got)
	}
	if ip.To4() == nil {
		t.Errorf("IPv4() = %q, not IPv4", got)
	}
	if ip.IsLoopback() {
		t.Errorf("IPv4() = %q, should not be loopback", got)
	}
}
