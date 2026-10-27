package demoports

import "testing"

// A missed or doubled offset would bind another process's listener or misroute
// the demonstration to that process's services.
func TestOffsetAddresses(t *testing.T) {
	t.Setenv("EAT_PORT_OFFSET", "1000")
	for address, want := range map[string]string{
		"127.0.0.2:9480": "127.0.0.2:10480", "127.0.0.1:9440": "127.0.0.1:10440",
		"127.0.0.1:9450": "127.0.0.1:10450", "127.0.0.1:9501": "127.0.0.1:10501",
		"127.0.0.1:9505": "127.0.0.1:10505", "127.0.0.1:9605": "127.0.0.1:10605",
		"[::1]:9480": "[::1]:10480",
	} {
		got, err := Offset(address)
		if err != nil || got != want {
			t.Errorf("Offset(%q): %q, %v; want %q", address, got, err, want)
		}
	}
	for _, offset := range []string{"-1", "x", "1.5", "65536", "65535", " 1000", "1000 "} {
		t.Setenv("EAT_PORT_OFFSET", offset)
		if got, err := Offset("127.0.0.1:9605"); err == nil {
			t.Errorf("invalid offset %q accepted: %q", offset, got)
		}
	}
	for _, offset := range []string{"", "0"} {
		t.Setenv("EAT_PORT_OFFSET", offset)
		if got, err := Offset("127.0.0.1:9440"); err != nil || got != "127.0.0.1:9440" {
			t.Errorf("zero offset: %q %v", got, err)
		}
	}
}
