package cloudflare

import "testing"

func TestParsePublicURL(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{line: "INF +--------------------------------------------------------------------------------------------+", ok: false},
		{line: "Your quick Tunnel has been created! Visit it at https://abc123.trycloudflare.com", want: "https://abc123.trycloudflare.com", ok: true},
		{line: "connected to https://example.com/path", want: "https://example.com/path", ok: true},
		{line: "http://example.com", ok: false},
	}
	for _, tc := range cases {
		got, ok := parsePublicURL(tc.line)
		if ok != tc.ok {
			t.Fatalf("line=%q expected ok=%t got %t", tc.line, tc.ok, ok)
		}
		if got != tc.want {
			t.Fatalf("line=%q expected url=%q got %q", tc.line, tc.want, got)
		}
	}
}
