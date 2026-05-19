package names

import (
	"testing"
	"unicode/utf8"
)

func TestGenerate_NotEmpty(t *testing.T) {
	name := Generate()
	if name == "" {
		t.Fatal("Generate() returned empty string")
	}
}

func TestGenerate_ValidUTF8(t *testing.T) {
	for i := 0; i < 100; i++ {
		name := Generate()
		if !utf8.ValidString(name) {
			t.Fatalf("Generate() returned invalid UTF-8: %q", name)
		}
	}
}

func TestGenerate_NoSuffixes(t *testing.T) {
	for i := 0; i < 200; i++ {
		name := Generate()
		for _, r := range name {
			if r >= '0' && r <= '9' {
				t.Fatalf("Generate() returned name with digit: %q", name)
			}
		}
	}
}

func TestGenerate_Distribution(t *testing.T) {
	seen := make(map[string]int)
	for i := 0; i < 1000; i++ {
		seen[Generate()]++
	}
	if len(seen) < 5 {
		t.Fatalf("Generate() returned only %d distinct names in 1000 calls", len(seen))
	}
}
