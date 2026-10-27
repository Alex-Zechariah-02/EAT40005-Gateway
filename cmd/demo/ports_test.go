package main

import (
	"testing"

	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/demoports"
)

func TestDemoAddressesRespectOffset(t *testing.T) {
	for _, entry := range []struct{ base, got string }{
		{"127.0.0.1:9440", adminListen}, {"127.0.0.2:9480", dataListen},
		{"127.0.0.1:9501", fixtures[0].listen}, {"127.0.0.1:9601", fixtures[0].control},
		{"127.0.0.1:9502", fixtures[1].listen}, {"127.0.0.1:9602", fixtures[1].control},
		{"127.0.0.1:9503", fixtures[2].listen}, {"127.0.0.1:9603", fixtures[2].control},
	} {
		want, err := demoports.Offset(entry.base)
		if err != nil || entry.got != want {
			t.Errorf("demo address %s: %s, want %s (%v)", entry.base, entry.got, want, err)
		}
	}
}
