// Package config decodes and validates server configuration.
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAbsoluteExpiryBudgetConfiguration(t *testing.T) {
	t.Setenv("ABSOLUTE_EXPIRY_MAX_IDENTITIES_PER_SHARD", "")
	cfg, err := LoadConfigOptional("")
	if err != nil || cfg.AbsoluteIdentityBudget != 1_000_000 {
		t.Fatalf("default=%+v %v", cfg, err)
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err = os.WriteFile(path, []byte("absoluteExpiryMaxIdentitiesPerShard: 1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil || cfg.AbsoluteIdentityBudget != 1234 {
		t.Fatalf("yaml=%+v %v", cfg, err)
	}
	t.Setenv("ABSOLUTE_EXPIRY_MAX_IDENTITIES_PER_SHARD", "2345")
	cfg, err = LoadConfig(path)
	if err != nil || cfg.AbsoluteIdentityBudget != 2345 {
		t.Fatalf("env=%+v %v", cfg, err)
	}
	t.Setenv("ABSOLUTE_EXPIRY_MAX_IDENTITIES_PER_SHARD", "invalid")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Validate() == nil {
		t.Fatal("invalid budget accepted")
	}
}
