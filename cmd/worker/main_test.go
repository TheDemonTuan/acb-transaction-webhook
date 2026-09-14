package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func TestWorkerQuiesceDrainFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	var receivedPath, receivedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedToken = r.Header.Get("X-Worker-Internal-Token")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"quiesced","quiesced":true}`))
	}))
	defer srv.Close()

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("worker -quiesce sends POST /rpc/quiesce with internal token", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-quiesce")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=test-worker-token-xyz",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("worker -quiesce failed: %v, stderr: %s", err, stderr.String())
		}
		if receivedPath != "/rpc/quiesce" {
			t.Errorf("expected path /rpc/quiesce, got %s", receivedPath)
		}
		if receivedToken != "test-worker-token-xyz" {
			t.Errorf("expected token test-worker-token-xyz, got %s", receivedToken)
		}
		if !strings.Contains(stdout.String(), `"quiesced":true`) {
			t.Errorf("expected quiesce response in stdout, got %s", stdout.String())
		}
	})

	t.Run("worker -quiesce sends POST /rpc/quiesce with WORKER_INTERNAL_TOKEN_FILE", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "worker_token")
		if err := os.WriteFile(tokenFile, []byte("file-token-secret-456\n"), 0600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("go", "run", ".", "-quiesce")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=",
			"WORKER_INTERNAL_TOKEN_FILE="+tokenFile,
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("worker -quiesce failed: %v, stderr: %s", err, stderr.String())
		}
		if receivedPath != "/rpc/quiesce" {
			t.Errorf("expected path /rpc/quiesce, got %s", receivedPath)
		}
		if receivedToken != "file-token-secret-456" {
			t.Errorf("expected token file-token-secret-456, got %s", receivedToken)
		}
	})

	t.Run("worker -resume sends POST /rpc/resume with WORKER_INTERNAL_TOKEN_FILE", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "worker_token")
		if err := os.WriteFile(tokenFile, []byte("resume-token-secret-789\n"), 0600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("go", "run", ".", "-resume")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=",
			"WORKER_INTERNAL_TOKEN_FILE="+tokenFile,
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("worker -resume failed: %v, stderr: %s", err, stderr.String())
		}
		if receivedPath != "/rpc/resume" {
			t.Errorf("expected path /rpc/resume, got %s", receivedPath)
		}
		if receivedToken != "resume-token-secret-789" {
			t.Errorf("expected token resume-token-secret-789, got %s", receivedToken)
		}
	})

	t.Run("worker -quiesce fails when WORKER_INTERNAL_TOKEN_FILE is missing", func(t *testing.T) {
		cmd := exec.Command("go", "run", ".", "-quiesce")
		cmd.Dir = "."
		cmd.Env = append(os.Environ(),
			"WORKER_PORT="+port,
			"WORKER_INTERNAL_TOKEN=",
			"WORKER_INTERNAL_TOKEN_FILE=/nonexistent/token/file",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err == nil {
			t.Fatal("expected error with nonexistent token file, got nil")
		}
		if !strings.Contains(stderr.String(), "failed to read worker internal token") {
			t.Fatalf("expected stderr to mention failed to read worker internal token, got %q", stderr.String())
		}
	})
}
