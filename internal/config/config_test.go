package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// chdir moves into dir for the test; testing.T.Chdir only exists from Go 1.24.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestLoadReadsDotEnvWithoutOverridingEnvironment(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	env := "COMFY_URL=http://10.0.0.5:8188/\nCOMFYVAULT_DATA=\"./vault\"\n# comment\nCOMFYVAULT_POLL_INTERVAL=250ms\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMFYVAULT_DATA", "/from/environment")
	os.Unsetenv("COMFY_URL")
	os.Unsetenv("COMFYVAULT_POLL_INTERVAL")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ComfyURL != "http://10.0.0.5:8188" {
		t.Errorf("ComfyURL = %q", c.ComfyURL)
	}
	if c.DataDir != "/from/environment" {
		t.Errorf("real environment must win over .env, got %q", c.DataDir)
	}
	if c.PollInterval != 250*time.Millisecond {
		t.Errorf("PollInterval = %v", c.PollInterval)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("COMFY_URL", "127.0.0.1:8188")
	if _, err := Load(); err == nil {
		t.Fatal("a URL without scheme should be rejected")
	}
	t.Setenv("COMFY_URL", "http://127.0.0.1:8188")
	t.Setenv("COMFYVAULT_RUN_TIMEOUT", "-3s")
	if _, err := Load(); err == nil {
		t.Fatal("a negative timeout should be rejected")
	}
}
