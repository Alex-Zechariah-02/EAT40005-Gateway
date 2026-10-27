package main

import (
	"testing"

	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/demoports"
)

func TestDemoAddressesRespectOffset(t *testing.T) {
	for _, entry := range []struct{ base, got string }{
		{"127.0.0.1:9440", adminListen}, {"127.0.0.2:9480", dataListen},
		{"127.0.0.1:9501", fetchServices[0].listen}, {"127.0.0.1:9601", fetchServices[0].control},
		{"127.0.0.1:9502", fetchServices[1].listen}, {"127.0.0.1:9602", fetchServices[1].control},
		{"127.0.0.1:9503", fetchServices[2].listen}, {"127.0.0.1:9603", fetchServices[2].control},
		{"127.0.0.1:9504", fetchServices[3].listen}, {"127.0.0.1:9604", fetchServices[3].control},
		{"127.0.0.1:9505", fetchServices[4].listen}, {"127.0.0.1:9605", fetchServices[4].control},
		{"127.0.0.1:9450", metricsListen},
	} {
		want, err := demoports.Offset(entry.base)
		if err != nil || entry.got != want {
			t.Errorf("demo address %s: %s, want %s (%v)", entry.base, entry.got, want, err)
		}
	}
}
