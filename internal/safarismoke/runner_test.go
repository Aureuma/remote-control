package safarismoke

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestResolveScenarios(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     []string
		wantErr  bool
		errMatch string
	}{
		{
			name: "default list",
			in:   "readwrite,readonly,access-code,no-token",
			want: []string{"readwrite", "readonly", "access-code", "no-token"},
		},
		{
			name: "dedupe and trim",
			in:   " readwrite,readonly,readwrite ",
			want: []string{"readwrite", "readonly"},
		},
		{
			name:     "unknown",
			in:       "readwrite,bad",
			wantErr:  true,
			errMatch: "unknown scenario",
		},
		{
			name:     "empty",
			in:       " , ",
			wantErr:  true,
			errMatch: "no scenarios selected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveScenarios(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				if tc.errMatch != "" && !strings.Contains(err.Error(), tc.errMatch) {
					t.Fatalf("error %q missing match %q", err, tc.errMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveScenarios error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got=%d want=%d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].name != tc.want[i] {
					t.Fatalf("scenario[%d]=%q want %q", i, got[i].name, tc.want[i])
				}
			}
		})
	}
}

func TestParseLabeledValue(t *testing.T) {
	line := "🌐 Share URL: https://example.com/abc"
	if got := parseLabeledValue(line, "Share URL:"); got != "https://example.com/abc" {
		t.Fatalf("got %q", got)
	}
	if got := parseLabeledValue("Access Token: tok_123", "Access Token:"); got != "tok_123" {
		t.Fatalf("got %q", got)
	}
	if got := parseLabeledValue("nope", "Share URL:"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestParseSessionLine(t *testing.T) {
	ev := parseSessionLine("🔑 Access Token: abc123")
	if ev.accessToken != "abc123" {
		t.Fatalf("token mismatch: %q", ev.accessToken)
	}
	ev = parseSessionLine("🌐 Share URL: http://127.0.0.1:8080/?token=abc")
	if ev.shareURL == "" {
		t.Fatal("expected share url")
	}
}

func TestExtractWDError(t *testing.T) {
	if err := extractWDError(200, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := extractWDError(404, json.RawMessage(`{"error":"no such alert","message":"none"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	var wdErr *wdError
	if !errors.As(err, &wdErr) {
		t.Fatalf("expected wdError, got %T", err)
	}
	if wdErr.code != "no such alert" {
		t.Fatalf("code=%q", wdErr.code)
	}

	err = extractWDError(500, nil)
	if err == nil {
		t.Fatal("expected error for empty non-2xx payload")
	}
}

func TestSessionID(t *testing.T) {
	id := sessionID("Read Write + Auth")
	if !strings.HasPrefix(id, "safari-smoke-read-write---auth-") {
		t.Fatalf("unexpected id prefix: %q", id)
	}
}

func TestShellQuoteAndJoin(t *testing.T) {
	if got := shellQuote("abc"); got != "'abc'" {
		t.Fatalf("shellQuote mismatch: %q", got)
	}
	if got := shellQuote("a'b"); got != "'a'\"'\"'b'" {
		t.Fatalf("shellQuote apostrophe mismatch: %q", got)
	}
	joined := shellJoin([]string{"./bin", "--name", "A B"})
	want := "'./bin' '--name' 'A B'"
	if joined != want {
		t.Fatalf("shellJoin mismatch: got %q want %q", joined, want)
	}
}
