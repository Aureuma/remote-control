package cloudflare

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParsePublicURL(t *testing.T) {
	cases := []struct {
		line string
		want string
		ok   bool
	}{
		{line: "INF +--------------------------------------------------------------------------------------------+", ok: false},
		{line: "Your quick Tunnel has been created! Visit it at https://abc123.trycloudflare.com", want: "https://abc123.trycloudflare.com", ok: true},
		{line: "INF |  https://alliance-naval-licenses-childrens.trycloudflare.com                               |", want: "https://alliance-naval-licenses-childrens.trycloudflare.com", ok: true},
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

func TestStartMissingBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Start(ctx, Options{
		Binary:         "definitely-missing-cloudflared-binary",
		LocalURL:       "http://127.0.0.1:8080",
		StartupTimeout: time.Second,
	})
	if err == nil {
		t.Fatalf("expected missing binary error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "binary not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildInvocationEphemeral(t *testing.T) {
	args, publicURL, err := buildInvocation("http://127.0.0.1:8080", Options{})
	if err != nil {
		t.Fatalf("buildInvocation: %v", err)
	}
	want := []string{"tunnel", "--url", "http://127.0.0.1:8080", "--no-autoupdate"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v want %v", args, want)
	}
	if publicURL != "" {
		t.Fatalf("publicURL=%q want empty", publicURL)
	}
}

func TestBuildInvocationNamedWithHostname(t *testing.T) {
	args, publicURL, err := buildInvocation("http://127.0.0.1:8080", Options{
		Mode:            "named",
		Hostname:        "rc.example.com",
		TunnelName:      "rc-tunnel",
		ConfigFile:      "/tmp/cloudflared.yml",
		CredentialsFile: "/tmp/creds.json",
	})
	if err != nil {
		t.Fatalf("buildInvocation named: %v", err)
	}
	want := []string{
		"tunnel",
		"--url", "http://127.0.0.1:8080",
		"--hostname", "rc.example.com",
		"--no-autoupdate",
		"--config", "/tmp/cloudflared.yml",
		"--credentials-file", "/tmp/creds.json",
		"--name", "rc-tunnel",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v want %v", args, want)
	}
	if publicURL != "https://rc.example.com" {
		t.Fatalf("publicURL=%q", publicURL)
	}
}

func TestBuildInvocationNamedWithToken(t *testing.T) {
	args, publicURL, err := buildInvocation("http://127.0.0.1:8080", Options{
		Mode:        "named",
		Hostname:    "rc.example.com",
		TunnelToken: "cf-token",
	})
	if err != nil {
		t.Fatalf("buildInvocation named token: %v", err)
	}
	want := []string{"tunnel", "run", "--token", "cf-token"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v want %v", args, want)
	}
	if publicURL != "https://rc.example.com" {
		t.Fatalf("publicURL=%q", publicURL)
	}
}

func TestBuildInvocationNamedRequiresHostname(t *testing.T) {
	_, _, err := buildInvocation("http://127.0.0.1:8080", Options{
		Mode: "named",
	})
	if err == nil {
		t.Fatalf("expected hostname error")
	}
}

func TestNormalizePublicURLFromHostname(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "rc.example.com", want: "https://rc.example.com", ok: true},
		{in: "https://rc.example.com/path", want: "https://rc.example.com", ok: true},
		{in: " ", ok: false},
	}
	for _, tc := range cases {
		got, err := normalizePublicURLFromHostname(tc.in)
		if tc.ok && err != nil {
			t.Fatalf("in=%q err=%v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("in=%q expected error", tc.in)
		}
		if tc.ok && got != tc.want {
			t.Fatalf("in=%q got=%q want=%q", tc.in, got, tc.want)
		}
	}
}
