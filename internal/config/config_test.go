package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearMikrotikEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MIKROTIK_ADDRESS",
		"MIKROTIK_USERNAME",
		"MIKROTIK_USERNAME_FILE",
		"MIKROTIK_PASSWORD",
		"MIKROTIK_PASSWORD_FILE",
		"MIKROTIK_USE_TLS",
		"MIKROTIK_SKIP_TLS_VERIFY",
		"MIKROTIK_DEFAULT_TTL",
		"MIKROTIK_ZONES",
		"WEBHOOK_LISTEN",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoad_HappyPath(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_ADDRESS", "192.168.1.1:8728")
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD", "s3cret")
	t.Setenv("MIKROTIK_ZONES", "home.lan, internal.example.com.")

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Address != "192.168.1.1:8728" || c.Username != "admin" || c.Password != "s3cret" {
		t.Fatalf("bad config: %+v", c)
	}
	if len(c.Zones) != 2 || c.Zones[0] != "home.lan" || c.Zones[1] != "internal.example.com" {
		t.Fatalf("zones not normalised: %+v", c.Zones)
	}
	if c.DefaultTTL != 3600 {
		t.Fatalf("default TTL should be 3600 by default, got %d", c.DefaultTTL)
	}
	if c.Listen != ":9090" {
		t.Fatalf("listen default wrong: %q", c.Listen)
	}
	if c.UseTLS || c.SkipTLSVerify {
		t.Fatalf("TLS flags should default false: %+v", c)
	}
}

func TestLoad_RejectsMissingAddress(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD", "s3cret")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MIKROTIK_ADDRESS") {
		t.Fatalf("expected MIKROTIK_ADDRESS error, got %v", err)
	}
}

func TestLoad_RejectsAddressWithoutPort(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_ADDRESS", "192.168.1.1")
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD", "s3cret")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("expected host:port error, got %v", err)
	}
}

func TestLoad_RejectsMissingCredentials(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_ADDRESS", "192.168.1.1:8728")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "MIKROTIK_USERNAME") {
		t.Fatalf("expected MIKROTIK_USERNAME error, got %v", err)
	}
}

func TestLoad_UsernameFileResolvedFromDisk(t *testing.T) {
	clearMikrotikEnv(t)
	dir := t.TempDir()
	uf := filepath.Join(dir, "user")
	pf := filepath.Join(dir, "pass")
	if err := os.WriteFile(uf, []byte("file-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pf, []byte("file-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIKROTIK_ADDRESS", "10.0.0.1:8728")
	t.Setenv("MIKROTIK_USERNAME_FILE", uf)
	t.Setenv("MIKROTIK_PASSWORD_FILE", pf)
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Username != "file-user" || c.Password != "file-pass" {
		t.Fatalf("file secrets not picked up: %+v", c)
	}
}

func TestLoad_RejectsBothUsernameAndUsernameFile(t *testing.T) {
	clearMikrotikEnv(t)
	dir := t.TempDir()
	uf := filepath.Join(dir, "user")
	if err := os.WriteFile(uf, []byte("file-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIKROTIK_ADDRESS", "10.0.0.1:8728")
	t.Setenv("MIKROTIK_USERNAME", "env-user")
	t.Setenv("MIKROTIK_USERNAME_FILE", uf)
	t.Setenv("MIKROTIK_PASSWORD", "p")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected mutex error, got %v", err)
	}
}

func TestLoad_RejectsEmptyPasswordFile(t *testing.T) {
	clearMikrotikEnv(t)
	dir := t.TempDir()
	pf := filepath.Join(dir, "pass")
	if err := os.WriteFile(pf, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIKROTIK_ADDRESS", "10.0.0.1:8728")
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD_FILE", pf)
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-file error, got %v", err)
	}
}

func TestLoad_TLSFlags(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_ADDRESS", "10.0.0.1:8729")
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD", "p")
	t.Setenv("MIKROTIK_USE_TLS", "true")
	t.Setenv("MIKROTIK_SKIP_TLS_VERIFY", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.UseTLS || !c.SkipTLSVerify {
		t.Fatalf("TLS flags not respected: %+v", c)
	}
}

func TestLoad_RejectsInvalidDefaultTTL(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_ADDRESS", "10.0.0.1:8728")
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD", "p")
	t.Setenv("MIKROTIK_DEFAULT_TTL", "-1")
	_, err := Load()
	if err == nil {
		t.Fatalf("expected error on negative TTL")
	}
}

func TestLoad_EmptyZonesMeansNoFilter(t *testing.T) {
	clearMikrotikEnv(t)
	t.Setenv("MIKROTIK_ADDRESS", "10.0.0.1:8728")
	t.Setenv("MIKROTIK_USERNAME", "admin")
	t.Setenv("MIKROTIK_PASSWORD", "p")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Zones != nil {
		t.Fatalf("zones should be nil when MIKROTIK_ZONES unset, got %+v", c.Zones)
	}
}
