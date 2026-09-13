package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseGatewayFlags_MigrateOnlyFails(t *testing.T) {
	var buf bytes.Buffer
	_, err := parseGatewayFlags([]string{"--migrate-only"}, &buf)
	if err == nil {
		t.Fatal("expected error when --migrate-only is provided, got nil")
	}
	if !strings.Contains(err.Error(), "dbtool --migrate") {
		t.Errorf("expected actionable error mentioning 'dbtool --migrate', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "--migrate-only") {
		t.Errorf("expected error mentioning '--migrate-only', got %q", err.Error())
	}
}

func TestParseGatewayFlags_ValidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want gatewayFlags
	}{
		{
			name: "empty args",
			args: []string{},
			want: gatewayFlags{},
		},
		{
			name: "healthcheck",
			args: []string{"--healthcheck"},
			want: gatewayFlags{healthcheck: true},
		},
		{
			name: "deploycheck",
			args: []string{"--deploycheck"},
			want: gatewayFlags{deploycheck: true},
		},
		{
			name: "check",
			args: []string{"--check"},
			want: gatewayFlags{checkIntegrity: true},
		},
		{
			name: "backup-to",
			args: []string{"--backup-to", "/tmp/backup.db"},
			want: gatewayFlags{backupTo: "/tmp/backup.db"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, err := parseGatewayFlags(tt.args, io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if flags.healthcheck != tt.want.healthcheck {
				t.Errorf("healthcheck: got %v, want %v", flags.healthcheck, tt.want.healthcheck)
			}
			if flags.checkIntegrity != tt.want.checkIntegrity {
				t.Errorf("checkIntegrity: got %v, want %v", flags.checkIntegrity, tt.want.checkIntegrity)
			}
			if flags.backupTo != tt.want.backupTo {
				t.Errorf("backupTo: got %q, want %q", flags.backupTo, tt.want.backupTo)
			}
		})
	}
}

func TestGatewayMigrateOnly_Subprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	cmd := exec.Command("go", "run", ".", "--migrate-only")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "LISTEN_ADDR=127.0.0.1:0")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected gateway --migrate-only subprocess to exit with error, got 0")
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "dbtool --migrate") {
		t.Errorf("expected stderr to contain 'dbtool --migrate', got: %s", stderrStr)
	}
}

func TestGatewayRoleValidation_Subprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	tests := []struct {
		name       string
		env        []string
		wantStderr string
	}{
		{
			name: "rejects worker role on gateway",
			env: []string{
				"APP_ENV=development",
				"RUNTIME_ROLE=worker",
			},
			wantStderr: "unsupported runtime role for gateway",
		},
		{
			name: "production rejects monolith-dev role",
			env: []string{
				"APP_ENV=production",
				"RUNTIME_ROLE=monolith-dev",
			},
			wantStderr: "monolith-dev role is forbidden in production",
		},
		{
			name: "production gateway rejects missing worker RPC URL",
			env: []string{
				"APP_ENV=production",
				"RUNTIME_ROLE=gateway",
				"APP_MASTER_KEY_FILE=/dev/null",
				"OWNER_SUBJECTS=owner@example.com",
				"CF_ACCESS_ISSUER=https://test.cloudflareaccess.com",
				"CF_ACCESS_AUDIENCE=aud123",
				"CF_ACCESS_JWKS_URL=https://test.cloudflareaccess.com/certs",
				"TTS_INTERNAL_TOKEN=token",
				"WORKER_RPC_URL=",
			},
			wantStderr: "WORKER_RPC_URL is required for gateway in production",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("go", "run", ".", "--healthcheck")
			// We run without healthcheck flag to exercise main role validation
			cmd = exec.Command("go", "run", ".")
			cmd.Dir = "."
			// Clean env plus test env
			cmd.Env = append(os.Environ(), tc.env...)

			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			err := cmd.Run()
			if err == nil {
				t.Fatalf("expected gateway startup to fail for %s, but it exited 0", tc.name)
			}
			out := stderr.String() + stdout.String()
			if !strings.Contains(out, tc.wantStderr) {
				t.Fatalf("expected output to contain %q, got %q", tc.wantStderr, out)
			}
		})
	}
}
