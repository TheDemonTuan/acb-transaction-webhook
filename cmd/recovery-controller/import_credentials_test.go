package main

import (
	"strings"
	"testing"
)

func TestImportPasswordPreservesSpacesAndRejectsEmbeddedControls(t *testing.T) {
	for _, input := range []string{" new-password ", " new-password \n", " new-password \r\n"} {
		got, err := parseImportCredential([]byte(input), true)
		if err != nil || got != " new-password " {
			t.Fatalf("password spaces changed: %q %v", got, err)
		}
	}
	for _, input := range []string{"", "\n", "pass\nword", "pass\rword", "pass\x00word", strings.Repeat("x", 1025)} {
		if _, err := parseImportCredential([]byte(input), true); err == nil {
			t.Fatal("invalid imported password admitted")
		}
	}
	user, err := parseImportCredential([]byte(" user \n"), false)
	if err != nil || user != "user" {
		t.Fatalf("username normalization: %q %v", user, err)
	}
}
