// Integration harness port allocation.

package gateway

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
)

// issuedPorts are the ports freeAddr has returned in this test process.
var issuedPorts = struct {
	sync.Mutex
	seen map[int]bool
}{seen: map[int]bool{}}

// freeAddr returns a loopback address that was free when checked, for a
// gateway to bind later. Ports come from 20000-29999, below the ranges Linux
// (32768-60999) and Windows (49152-65535) give to outgoing connections, so a
// connection the tests open cannot take the port in the meantime, and no
// port is returned twice.
func freeAddr(t *testing.T) string {
	t.Helper()
	issuedPorts.Lock()
	defer issuedPorts.Unlock()
	for range 1000 {
		port := 20000 + rand.IntN(10000)
		if issuedPorts.seen[port] {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // in use, or reserved by the host
		}
		l.Close()
		issuedPorts.seen[port] = true
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	t.Fatal("no free loopback port in 20000-29999")
	return ""
}
