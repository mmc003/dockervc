package cli

import (
	"strings"
	"testing"
)

func TestConfigUnsetZstdLevelRestoresDefault(t *testing.T) {
	seedStore(t, 1)

	captureOutput(func() { runArgs("config", "set", "zstd_level", "9") })
	out := captureOutput(func() { runArgs("config", "unset", "zstd_level") })
	if !strings.Contains(out, "zstd_level unset") {
		t.Fatalf("unset output = %q", out)
	}
	out = captureOutput(func() { runArgs("config", "get", "zstd_level") })
	if strings.TrimSpace(out) != "3" {
		t.Fatalf("zstd_level after unset = %q, want 3", out)
	}
}
