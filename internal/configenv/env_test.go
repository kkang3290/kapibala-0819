package configenv

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStringPrefersSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "from-env")
	t.Setenv("TEST_SECRET_FILE", path)
	value, err := String("TEST_SECRET", "fallback")
	if err != nil || value != "from-file" {
		t.Fatalf("value=%q err=%v", value, err)
	}
}

func TestInvalidDurationFails(t *testing.T) {
	t.Setenv("TEST_DURATION", "not-a-duration")
	if _, err := Duration("TEST_DURATION", time.Second); err == nil {
		t.Fatal("invalid duration was accepted")
	}
}
