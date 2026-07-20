package tunnel

import (
	"reflect"
	"testing"
)

func TestRedactEnvValuesInArgv(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "docker exec with -e pairs",
			in: []string{
				"docker", "exec", "-i", "-e", "API_TOKEN=sk-secret-123", "-e", "HOME=/root",
				"-w", "/workspace", "container-1", "bash", "-lc", "echo hi",
			},
			want: []string{
				"docker", "exec", "-i", "-e", "API_TOKEN=[REDACTED]", "-e", "HOME=[REDACTED]",
				"-w", "/workspace", "container-1", "bash", "-lc", "echo hi",
			},
		},
		{
			name: "long --env separate token",
			in:   []string{"docker", "exec", "--env", "SECRET=hunter2", "c", "cmd"},
			want: []string{"docker", "exec", "--env", "SECRET=[REDACTED]", "c", "cmd"},
		},
		{
			name: "--env= single token",
			in:   []string{"docker", "exec", "--env=SECRET=hunter2", "c", "cmd"},
			want: []string{"docker", "exec", "--env=SECRET=[REDACTED]", "c", "cmd"},
		},
		{
			name: "-e= single token",
			in:   []string{"docker", "exec", "-e=SECRET=hunter2", "c", "cmd"},
			want: []string{"docker", "exec", "-e=SECRET=[REDACTED]", "c", "cmd"},
		},
		{
			name: "value containing further equals splits at first",
			in:   []string{"-e", "JWT=eyJ.a=b=c"},
			want: []string{"-e", "JWT=[REDACTED]"},
		},
		{
			name: "inherit-from-environment form has no value to redact",
			in:   []string{"docker", "exec", "-e", "PATH", "c", "cmd"},
			want: []string{"docker", "exec", "-e", "PATH", "c", "cmd"},
		},
		{
			name: "trailing -e without argument",
			in:   []string{"docker", "exec", "-e"},
			want: []string{"docker", "exec", "-e"},
		},
		{
			name: "unrelated tokens with equals are untouched",
			in:   []string{"env", "FOO=bar", "sh", "-c", "x=1; echo $x"},
			want: []string{"env", "FOO=bar", "sh", "-c", "x=1; echo $x"},
		},
		{
			name: "empty argv",
			in:   nil,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inCopy := append([]string(nil), tc.in...)
			got := redactEnvValuesInArgv(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("redactEnvValuesInArgv(%v) = %v, want %v", tc.in, got, tc.want)
			}
			// The original argv is still used to actually spawn the process, so
			// redaction must operate on a copy and never mutate its input.
			if !reflect.DeepEqual(tc.in, inCopy) {
				t.Fatalf("input argv was mutated: %v (was %v)", tc.in, inCopy)
			}
		})
	}
}

func TestRedactEnvAssignmentKeepsKeyOnly(t *testing.T) {
	if got := redactEnvAssignment("KEY=value"); got != "KEY=[REDACTED]" {
		t.Fatalf("got %q", got)
	}
	if got := redactEnvAssignment("KEY="); got != "KEY=[REDACTED]" {
		t.Fatalf("empty value must still be masked, got %q", got)
	}
	if got := redactEnvAssignment("KEY"); got != "KEY" {
		t.Fatalf("no-value token must pass through, got %q", got)
	}
}
