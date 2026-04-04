package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-redis/redis/v8"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/osvaldoandrade/codeq/internal/shard"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

func migrateCmd(ui *ui) *cobra.Command {
	var (
		configFile string
		command    string
		tenantID   string
		fromShard  string
		toShard    string
		batchSize  int64
		dryRun     bool
		verify     bool
	)

	migrate := &cobra.Command{
		Use:   "migrate-shards",
		Short: "Migrate tasks between shards",
		Long: `Migrate tasks from one shard to another for a given command.

Moves all queue types (pending, delayed, in-progress, DLQ) from the
source shard to the destination shard, preserving task data and ordering.

Requires a codeQ server configuration file with sharding backends defined.`,
		Example: `  # Dry-run to preview migration
  codeq migrate-shards --config config.yml --command GENERATE_MASTER \
      --from-shard default --to-shard compute-shard --dry-run

  # Execute migration with verification
  codeq migrate-shards --config config.yml --command GENERATE_MASTER \
      --from-shard default --to-shard compute-shard --verify

  # Migrate tenant-specific tasks
  codeq migrate-shards --config config.yml --command GENERATE_MASTER \
      --tenant tenant-abc --from-shard default --to-shard premium-shard`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if configFile == "" {
				return fmt.Errorf("--config is required")
			}
			if command == "" {
				return fmt.Errorf("--command is required")
			}
			if fromShard == "" {
				return fmt.Errorf("--from-shard is required")
			}
			if toShard == "" {
				return fmt.Errorf("--to-shard is required")
			}

			cfg, err := loadServerConfig(configFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if !cfg.Sharding.Enabled || len(cfg.Sharding.Backends) == 0 {
				return fmt.Errorf("sharding is not enabled or no backends configured in %s", configFile)
			}

			// Validate that both shards exist in the config
			if _, ok := cfg.Sharding.Backends[fromShard]; !ok {
				return fmt.Errorf("source shard %q not found in config backends", fromShard)
			}
			if _, ok := cfg.Sharding.Backends[toShard]; !ok {
				return fmt.Errorf("destination shard %q not found in config backends", toShard)
			}

			// Build Redis clients
			clients, err := buildClientMap(cfg.Sharding)
			if err != nil {
				return fmt.Errorf("build client map: %w", err)
			}
			defer func() { _ = clients.Close() }()

			ctx := context.Background()

			// Health check before migration
			fmt.Fprintln(os.Stderr, ui.info("Checking shard health..."))
			health := shard.HealthCheck(ctx, clients)
			allHealthy := true
			for sid, ok := range health {
				if ok {
					fmt.Fprintf(os.Stderr, "  %s %s\n", ui.ok("✓"), sid)
				} else {
					fmt.Fprintf(os.Stderr, "  %s %s\n", ui.err("✗"), sid)
					allHealthy = false
				}
			}
			if !allHealthy {
				return fmt.Errorf("one or more shards are unhealthy; aborting migration")
			}

			if dryRun {
				fmt.Fprintln(os.Stderr, ui.warn("[DRY-RUN]"), "No changes will be made")
			}

			// Setup progress bar
			bar := progressbar.NewOptions64(
				-1,
				progressbar.OptionSetDescription("Migrating tasks"),
				progressbar.OptionSetWriter(os.Stderr),
				progressbar.OptionShowCount(),
				progressbar.OptionSetWidth(40),
				progressbar.OptionClearOnFinish(),
			)

			fmt.Fprintf(os.Stderr, "\n%s %s → %s (command: %s",
				ui.info("Migration:"), fromShard, toShard, strings.ToUpper(command))
			if tenantID != "" {
				fmt.Fprintf(os.Stderr, ", tenant: %s", tenantID)
			}
			fmt.Fprintln(os.Stderr, ")")

			opts := shard.MigrateOptions{
				Command:   command,
				TenantID:  tenantID,
				FromShard: fromShard,
				ToShard:   toShard,
				BatchSize: batchSize,
				DryRun:    dryRun,
				OnProgress: func(p shard.MigrateProgress) {
					_ = bar.Set64(p.Migrated)
					bar.Describe(fmt.Sprintf("Migrating %s", p.QueueType))
				},
			}

			res, err := shard.Migrate(ctx, clients, opts)
			_ = bar.Finish()
			fmt.Fprintln(os.Stderr)

			if err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			// Print results
			modeLabel := "Migrated"
			if dryRun {
				modeLabel = "Would migrate"
			}
			fmt.Fprintf(os.Stderr, "\n%s\n", ui.title("Results"))
			fmt.Fprintf(os.Stderr, "  Pending:     %s %d tasks\n", modeLabel, res.PendingMigrated)
			fmt.Fprintf(os.Stderr, "  Delayed:     %s %d tasks\n", modeLabel, res.DelayedMigrated)
			fmt.Fprintf(os.Stderr, "  In-Progress: %s %d tasks\n", modeLabel, res.InProgMigrated)
			fmt.Fprintf(os.Stderr, "  DLQ:         %s %d tasks\n", modeLabel, res.DLQMigrated)
			fmt.Fprintf(os.Stderr, "  %s: %s %d tasks in %s\n",
				ui.ok("Total"), modeLabel, res.TotalMigrated, res.Elapsed.Round(1000000))

			// Post-migration verification
			if verify && !dryRun {
				fmt.Fprintf(os.Stderr, "\n%s\n", ui.info("Verifying migration..."))
				vr, err := shard.Verify(ctx, clients, command, tenantID, fromShard, toShard)
				if err != nil {
					return fmt.Errorf("verification failed: %w", err)
				}

				fmt.Fprintf(os.Stderr, "  Source remaining:  pending=%d delayed=%d inprog=%d dlq=%d\n",
					vr.SourceCounts["pending"], vr.SourceCounts["delayed"],
					vr.SourceCounts["inprog"], vr.SourceCounts["dlq"])
				fmt.Fprintf(os.Stderr, "  Dest counts:       pending=%d delayed=%d inprog=%d dlq=%d\n",
					vr.DestCounts["pending"], vr.DestCounts["delayed"],
					vr.DestCounts["inprog"], vr.DestCounts["dlq"])

				for sid, ok := range vr.Healthy {
					status := ui.ok("healthy")
					if !ok {
						status = ui.err("unhealthy")
					}
					fmt.Fprintf(os.Stderr, "  Shard %s: %s\n", sid, status)
				}

				if vr.OK {
					fmt.Fprintf(os.Stderr, "\n%s Migration verified successfully\n", ui.ok("✓"))
				} else {
					fmt.Fprintf(os.Stderr, "\n%s Verification found issues — review counts above\n", ui.warn("⚠"))
				}
			}

			return nil
		},
	}

	migrate.Flags().StringVar(&configFile, "config", "", "Path to codeQ server configuration file (required)")
	migrate.Flags().StringVar(&command, "command", "", "Command to migrate (required)")
	migrate.Flags().StringVar(&tenantID, "tenant", "", "Tenant ID (optional, for tenant-specific migration)")
	migrate.Flags().StringVar(&fromShard, "from-shard", "", "Source shard identifier (required)")
	migrate.Flags().StringVar(&toShard, "to-shard", "", "Destination shard identifier (required)")
	migrate.Flags().Int64Var(&batchSize, "batch-size", 1000, "Number of tasks per batch")
	migrate.Flags().BoolVar(&dryRun, "dry-run", false, "Preview migration without making changes")
	migrate.Flags().BoolVar(&verify, "verify", false, "Run post-migration verification")

	return migrate
}

// loadServerConfig loads a codeQ server configuration file.
func loadServerConfig(path string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// buildClientMap creates a shard.ClientMap from the sharding configuration.
func buildClientMap(sc config.ShardingConfig) (*shard.ClientMap, error) {
	clients := make(map[string]*redis.Client, len(sc.Backends))
	for name, backend := range sc.Backends {
		poolSize := backend.PoolSize
		if poolSize <= 0 {
			poolSize = 10
		}
		clients[name] = redis.NewClient(&redis.Options{
			Addr:     backend.Address,
			Password: backend.Password,
			DB:       backend.DB,
			PoolSize: poolSize,
		})
	}
	defaultShard := sc.DefaultShard
	if defaultShard == "" {
		defaultShard = shard.DefaultShardID
	}
	return shard.NewClientMap(clients, defaultShard)
}


