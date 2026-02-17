package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigOptional_EmptyPath tests loading when file path is empty
func TestLoadConfigOptional_EmptyPath(t *testing.T) {
	// Set environment variable to verify env override works with empty path
	t.Setenv("PORT", "9999")

	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatalf("LoadConfigOptional with empty path should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}
	// Verify environment variable was applied
	if cfg.Port != 9999 {
		t.Errorf("Expected Port=9999 from env, got %d", cfg.Port)
	}
}

// TestLoadConfigOptional_WhitespacePath tests loading when file path is only whitespace
func TestLoadConfigOptional_WhitespacePath(t *testing.T) {
	cfg, err := LoadConfigOptional("   ")
	if err != nil {
		t.Fatalf("LoadConfigOptional with whitespace path should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}
}

// TestLoadConfigOptional_FileNotExist tests loading when file does not exist
func TestLoadConfigOptional_FileNotExist(t *testing.T) {
	// Use a non-existent path within a valid temp directory for cross-platform compatibility
	nonExistentPath := filepath.Join(t.TempDir(), "config-does-not-exist.yaml")

	cfg, err := LoadConfigOptional(nonExistentPath)
	if err != nil {
		t.Fatalf("LoadConfigOptional with non-existent file should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}
}

// TestLoadConfigOptional_InvalidYAML tests loading when file exists but has invalid YAML
func TestLoadConfigOptional_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "invalid.yaml")

	// Write invalid YAML
	invalidYAML := `
port: 8080
redisAddr: "localhost:6379"
  invalid indentation here
  more bad yaml
`
	if err := os.WriteFile(configPath, []byte(invalidYAML), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	_, err := LoadConfigOptional(configPath)
	if err == nil {
		t.Fatal("Expected error when loading invalid YAML, got nil")
	}
}

// TestLoadConfigOptional_ValidConfig tests loading when file exists with valid config
func TestLoadConfigOptional_ValidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "valid.yaml")

	// Write valid YAML
	validYAML := `
port: 8080
redisAddr: "localhost:6379"
redisPassword: "secret"
identityServiceUrl: "http://identity:8080"
logLevel: "info"
env: "test"
`
	if err := os.WriteFile(configPath, []byte(validYAML), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath)
	if err != nil {
		t.Fatalf("LoadConfigOptional with valid config should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}

	// Verify values from file were loaded
	if cfg.Port != 8080 {
		t.Errorf("Expected Port=8080, got %d", cfg.Port)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("Expected RedisAddr='localhost:6379', got %q", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "secret" {
		t.Errorf("Expected RedisPassword='secret', got %q", cfg.RedisPassword)
	}
	if cfg.IdentityServiceURL != "http://identity:8080" {
		t.Errorf("Expected IdentityServiceURL='http://identity:8080', got %q", cfg.IdentityServiceURL)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("Expected LogLevel='info', got %q", cfg.LogLevel)
	}
	if cfg.Env != "test" {
		t.Errorf("Expected Env='test', got %q", cfg.Env)
	}
}

// TestLoadConfigOptional_EnvOverrides tests that environment variables override file values
func TestLoadConfigOptional_EnvOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	// Write config file
	configYAML := `
port: 8080
redisAddr: "localhost:6379"
redisPassword: "file-password"
identityServiceUrl: "http://file-identity:8080"
logLevel: "info"
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Set environment variables that should override file values
	t.Setenv("PORT", "9090")
	t.Setenv("REDIS_ADDR", "env-redis:6380")
	t.Setenv("REDIS_PASSWORD", "env-password")
	t.Setenv("IDENTITY_SERVICE_URL", "http://env-identity:9090")

	cfg, err := LoadConfigOptional(configPath)
	if err != nil {
		t.Fatalf("LoadConfigOptional should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}

	// Verify environment variables override file values
	if cfg.Port != 9090 {
		t.Errorf("Expected Port=9090 from env, got %d", cfg.Port)
	}
	if cfg.RedisAddr != "env-redis:6380" {
		t.Errorf("Expected RedisAddr='env-redis:6380' from env, got %q", cfg.RedisAddr)
	}
	if cfg.RedisPassword != "env-password" {
		t.Errorf("Expected RedisPassword='env-password' from env, got %q", cfg.RedisPassword)
	}
	if cfg.IdentityServiceURL != "http://env-identity:9090" {
		t.Errorf("Expected IdentityServiceURL='http://env-identity:9090' from env, got %q", cfg.IdentityServiceURL)
	}
}

// TestLoadConfigOptional_EnvOverridesEmptyFile tests env overrides work when file path is empty
func TestLoadConfigOptional_EnvOverridesEmptyFile(t *testing.T) {
	// Set multiple environment variables
	t.Setenv("PORT", "7070")
	t.Setenv("REDIS_ADDR", "redis.local:6379")
	t.Setenv("IDENTITY_SERVICE_API_KEY", "test-api-key")
	t.Setenv("PRODUCER_AUTH_PROVIDER", "static")

	cfg, err := LoadConfigOptional("")
	if err != nil {
		t.Fatalf("LoadConfigOptional with empty path should not error: %v", err)
	}

	// Verify environment variables were applied
	if cfg.Port != 7070 {
		t.Errorf("Expected Port=7070 from env, got %d", cfg.Port)
	}
	if cfg.RedisAddr != "redis.local:6379" {
		t.Errorf("Expected RedisAddr='redis.local:6379' from env, got %q", cfg.RedisAddr)
	}
	if cfg.IdentityServiceApiKey != "test-api-key" {
		t.Errorf("Expected IdentityServiceApiKey='test-api-key' from env, got %q", cfg.IdentityServiceApiKey)
	}
	if cfg.ProducerAuthProvider != "static" {
		t.Errorf("Expected ProducerAuthProvider='static' from env, got %q", cfg.ProducerAuthProvider)
	}
}
