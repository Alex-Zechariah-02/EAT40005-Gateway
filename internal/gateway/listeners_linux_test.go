package gateway

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// listeningPorts lists the TCP ports a process listens on, using ss from
// iproute2; a socket can show several owners, so the pid is matched exactly.
func listeningPorts(t *testing.T, pid int) []string {
	t.Helper()
	out, err := exec.Command("ss", "-H", "-l", "-t", "-n", "-p").Output()
	if err != nil {
		t.Fatalf("ss: %v", err)
	}
	owner := "pid=" + strconv.Itoa(pid) + ","
	var ports []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[0] == "LISTEN" && strings.Contains(fields[5], owner) {
			ports = append(ports, port(fields[3]))
		}
	}
	return ports
}
