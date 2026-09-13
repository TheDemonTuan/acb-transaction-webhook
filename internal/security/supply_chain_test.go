package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

type cveEntry struct {
	CVE    string `json:"cve"`
	Reason string `json:"reason"`
	Owner  string `json:"owner"`
	Expiry string `json:"expiry"`
}

type cveAllowlist struct {
	Version    int        `json:"version"`
	Exceptions []cveEntry `json:"exceptions"`
}

type thirdPartyImageEntry struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
	Owner  string `json:"owner"`
	Reason string `json:"reason"`
	Expiry string `json:"expiry"`
}

type thirdPartyAllowlist struct {
	Version int                    `json:"version"`
	Images  []thirdPartyImageEntry `json:"images"`
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root with go.mod")
		}
		dir = parent
	}
}

func TestCVEAllowlistFormatAndValidity(t *testing.T) {
	t.Parallel()
	repoRoot := findRepoRoot(t)
	allowlistPath := filepath.Join(repoRoot, "deploy", "cve-allowlist.json")

	data, err := os.ReadFile(allowlistPath)
	if err != nil {
		t.Fatalf("read cve-allowlist.json: %v", err)
	}

	var al cveAllowlist
	if err := json.Unmarshal(data, &al); err != nil {
		t.Fatalf("unmarshal cve-allowlist.json: %v", err)
	}

	cvePattern := regexp.MustCompile(`^(CVE-[0-9]{4}-[0-9]+|GHSA-[0-9a-zA-Z]{4}-[0-9a-zA-Z]{4}-[0-9a-zA-Z]{4})$`)
	now := time.Now().UTC()

	for idx, ex := range al.Exceptions {
		if !cvePattern.MatchString(ex.CVE) {
			t.Errorf("exception #%d invalid CVE pattern: %s", idx, ex.CVE)
		}
		if ex.Reason == "" {
			t.Errorf("exception #%d (%s) missing reason", idx, ex.CVE)
		}
		if ex.Owner == "" {
			t.Errorf("exception #%d (%s) missing owner", idx, ex.CVE)
		}
		expDate, err := time.Parse("2006-01-02", ex.Expiry)
		if err != nil {
			t.Errorf("exception #%d (%s) invalid expiry format: %s (%v)", idx, ex.CVE, ex.Expiry, err)
		} else if expDate.Before(now.Truncate(24 * time.Hour)) {
			t.Errorf("exception #%d (%s) expired on %s", idx, ex.CVE, ex.Expiry)
		}
	}
}

func TestThirdPartyBarkAllowlistFormatAndValidity(t *testing.T) {
	t.Parallel()
	repoRoot := findRepoRoot(t)
	allowlistPath := filepath.Join(repoRoot, "deploy", "third-party-allowlist.json")

	data, err := os.ReadFile(allowlistPath)
	if err != nil {
		t.Fatalf("read third-party-allowlist.json: %v", err)
	}

	var al thirdPartyAllowlist
	if err := json.Unmarshal(data, &al); err != nil {
		t.Fatalf("unmarshal third-party-allowlist.json: %v", err)
	}

	digestPattern := regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	now := time.Now().UTC()

	if len(al.Images) == 0 {
		t.Fatal("third-party allowlist has no approved images")
	}

	for idx, item := range al.Images {
		if !digestPattern.MatchString(item.Digest) {
			t.Errorf("image entry #%d (%s) invalid digest format: %s", idx, item.Image, item.Digest)
		}
		if item.Owner == "" {
			t.Errorf("image entry #%d (%s) missing owner", idx, item.Image)
		}
		if item.Reason == "" {
			t.Errorf("image entry #%d (%s) missing reason", idx, item.Image)
		}
		expDate, err := time.Parse("2006-01-02", item.Expiry)
		if err != nil {
			t.Errorf("image entry #%d (%s) invalid expiry format: %s (%v)", idx, item.Image, item.Expiry, err)
		} else if expDate.Before(now.Truncate(24 * time.Hour)) {
			t.Errorf("image entry #%d (%s) expired on %s", idx, item.Image, item.Expiry)
		}
	}
}

func TestManifestChecksumVerificationLogic(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	testFile := filepath.Join(tmp, "artifact.txt")
	content := []byte("hello reproducible world\n")
	if err := os.WriteFile(testFile, content, 0o644); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(content)
	expectedHex := hex.EncodeToString(sum[:])

	// Read and verify
	readContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatal(err)
	}
	actualSum := sha256.Sum256(readContent)
	actualHex := hex.EncodeToString(actualSum[:])

	if actualHex != expectedHex {
		t.Fatalf("checksum mismatch: expected %s got %s", expectedHex, actualHex)
	}

	// Tampered content
	tamperedContent := append(content, []byte("tamper")...)
	tamperedSum := sha256.Sum256(tamperedContent)
	tamperedHex := hex.EncodeToString(tamperedSum[:])
	if tamperedHex == expectedHex {
		t.Fatal("expected tamper detection")
	}
}
