package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestWorkerRoleValidation_Subprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	tests := []struct {
		name       string
		env        []string
		wantStderr string
	}{
		{
			name: "rejects gateway role on worker",
			env: []string{
				"APP_ENV=development",
				"RUNTIME_ROLE=gateway",
			},
			wantStderr: "unsupported runtime role for worker",
		},
		{
			name: "rejects monolith-dev role on worker",
			env: []string{
				"APP_ENV=development",
				"RUNTIME_ROLE=monolith-dev",
			},
			wantStderr: "unsupported runtime role for worker",
		},
		{
			name: "production rejects empty role on worker",
			env: []string{
				"APP_ENV=production",
				"RUNTIME_ROLE=",
			},
			wantStderr: "RUNTIME_ROLE is required in production",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("go", "run", ".")
			cmd.Dir = "."
			cmd.Env = append(os.Environ(), tc.env...)

			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("expected worker startup to fail for %s, but it exited 0", tc.name)
			}
			out := stderr.String() + stdout.String()
			if !strings.Contains(out, tc.wantStderr) {
				t.Fatalf("expected output to contain %q, got %q", tc.wantStderr, out)
			}
		})
	}
}
