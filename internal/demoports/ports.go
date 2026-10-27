package demoports

import (
	"errors"
	"net/netip"
	"os"
	"strconv"
)

// Offset applies EAT_PORT_OFFSET once to a base address. Empty means zero;
// malformed values, negative values and port overflow are errors.
func Offset(address string) (string, error) {
	base, err := netip.ParseAddrPort(address)
	if err != nil || base.Port() == 0 || !base.Addr().IsLoopback() {
		return "", errors.New("demo address must be a literal loopback address with a nonzero port")
	}
	value := os.Getenv("EAT_PORT_OFFSET")
	var offset uint64
	if value != "" {
		offset, err = strconv.ParseUint(value, 10, 16)
		if err != nil {
			return "", errors.New("EAT_PORT_OFFSET must be a nonnegative decimal integer")
		}
	}
	port := uint64(base.Port()) + offset
	if port > 65535 {
		return "", errors.New("EAT_PORT_OFFSET exceeds the available port range")
	}
	return netip.AddrPortFrom(base.Addr(), uint16(port)).String(), nil
}
