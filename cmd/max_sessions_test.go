package cmd

import (
	"strings"
	"testing"
)

func TestResolveMaxSessionsPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		flagValue   int
		envValue    string
		modeDefault int
		want        int
		wantErr     string
	}{
		{name: "flag wins over env and default", flagValue: 7, envValue: "99", modeDefault: 128, want: 7},
		{name: "explicit flag zero means unlimited", flagValue: 0, envValue: "99", modeDefault: 128, want: 0},
		{name: "env wins over default when flag unset", flagValue: -1, envValue: "42", modeDefault: 128, want: 42},
		{name: "env zero means unlimited", flagValue: -1, envValue: "0", modeDefault: 128, want: 0},
		{name: "mode default when nothing set", flagValue: -1, envValue: "", modeDefault: 1 << 20, want: 1 << 20},
		{name: "non-numeric env is rejected", flagValue: -1, envValue: "lots", modeDefault: 128, wantErr: "TAURUS_RELAY_MAX_SESSIONS"},
		{name: "negative env is rejected", flagValue: -1, envValue: "-5", modeDefault: 128, wantErr: "non-negative"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMaxSessions(tc.flagValue, tc.envValue, tc.modeDefault)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, got)
			}
		})
	}
}
