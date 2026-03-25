package shard

import "testing"

func TestShardKeySegment(t *testing.T) {
	tests := []struct {
		name    string
		shardID string
		want    string
	}{
		{"empty shard", "", ""},
		{"default shard", DefaultShardID, ""},
		{"named shard", "compute-heavy", ":s:compute-heavy"},
		{"primary shard", "primary", ":s:primary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShardKeySegment(tt.shardID)
			if got != tt.want {
				t.Errorf("ShardKeySegment(%q) = %q, want %q", tt.shardID, got, tt.want)
			}
		})
	}
}

func TestQueueKeyPending(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		tenantID string
		shardID  string
		priority int
		want     string
	}{
		{
			name:    "no tenant, no shard (legacy)",
			command: "GENERATE_MASTER", tenantID: "", shardID: "", priority: 5,
			want: "codeq:q:generate_master:pending:5",
		},
		{
			name:    "no tenant, default shard (backward compat)",
			command: "GENERATE_MASTER", tenantID: "", shardID: DefaultShardID, priority: 5,
			want: "codeq:q:generate_master:pending:5",
		},
		{
			name:    "with tenant, no shard",
			command: "GENERATE_MASTER", tenantID: "tenant-123", shardID: "", priority: 3,
			want: "codeq:q:generate_master:tenant-123:pending:3",
		},
		{
			name:    "with tenant and shard",
			command: "GENERATE_MASTER", tenantID: "tenant-123", shardID: "compute-heavy", priority: 5,
			want: "codeq:q:generate_master:tenant-123:s:compute-heavy:pending:5",
		},
		{
			name:    "no tenant, named shard",
			command: "SEND_EMAIL", tenantID: "", shardID: "notification", priority: 0,
			want: "codeq:q:send_email:s:notification:pending:0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QueueKeyPending(tt.command, tt.tenantID, tt.shardID, tt.priority)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueueKeyInProgress(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		tenantID string
		shardID  string
		want     string
	}{
		{
			name: "legacy", command: "CMD", tenantID: "", shardID: "",
			want: "codeq:q:cmd:inprog",
		},
		{
			name: "with tenant", command: "CMD", tenantID: "t1", shardID: "",
			want: "codeq:q:cmd:t1:inprog",
		},
		{
			name: "with shard", command: "CMD", tenantID: "t1", shardID: "s1",
			want: "codeq:q:cmd:t1:s:s1:inprog",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QueueKeyInProgress(tt.command, tt.tenantID, tt.shardID)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueueKeyDelayed(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		tenantID string
		shardID  string
		want     string
	}{
		{
			name: "legacy", command: "CMD", tenantID: "", shardID: "",
			want: "codeq:q:cmd:delayed",
		},
		{
			name: "with shard", command: "CMD", tenantID: "", shardID: "s1",
			want: "codeq:q:cmd:s:s1:delayed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QueueKeyDelayed(tt.command, tt.tenantID, tt.shardID)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueueKeyDLQ(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		tenantID string
		shardID  string
		want     string
	}{
		{
			name: "legacy", command: "CMD", tenantID: "", shardID: "",
			want: "codeq:q:cmd:dlq",
		},
		{
			name: "with tenant and shard", command: "CMD", tenantID: "t1", shardID: "s1",
			want: "codeq:q:cmd:t1:s:s1:dlq",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := QueueKeyDLQ(tt.command, tt.tenantID, tt.shardID)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackwardCompatibility_KeysMatchLegacyFormat(t *testing.T) {
	// Verify that keys generated with empty/default shard match existing format exactly.
	// This is critical for backward compatibility with existing deployments.
	legacy := map[string]string{
		"pending_no_tenant":  "codeq:q:generate_master:pending:5",
		"pending_tenant":     "codeq:q:generate_master:tenant-123:pending:5",
		"inprog_no_tenant":   "codeq:q:generate_master:inprog",
		"inprog_tenant":      "codeq:q:generate_master:tenant-123:inprog",
		"delayed_no_tenant":  "codeq:q:generate_master:delayed",
		"delayed_tenant":     "codeq:q:generate_master:tenant-123:delayed",
		"dlq_no_tenant":      "codeq:q:generate_master:dlq",
		"dlq_tenant":         "codeq:q:generate_master:tenant-123:dlq",
	}

	actual := map[string]string{
		"pending_no_tenant":  QueueKeyPending("GENERATE_MASTER", "", "", 5),
		"pending_tenant":     QueueKeyPending("GENERATE_MASTER", "tenant-123", "", 5),
		"inprog_no_tenant":   QueueKeyInProgress("GENERATE_MASTER", "", ""),
		"inprog_tenant":      QueueKeyInProgress("GENERATE_MASTER", "tenant-123", ""),
		"delayed_no_tenant":  QueueKeyDelayed("GENERATE_MASTER", "", ""),
		"delayed_tenant":     QueueKeyDelayed("GENERATE_MASTER", "tenant-123", ""),
		"dlq_no_tenant":      QueueKeyDLQ("GENERATE_MASTER", "", ""),
		"dlq_tenant":         QueueKeyDLQ("GENERATE_MASTER", "tenant-123", ""),
	}

	for key, want := range legacy {
		got := actual[key]
		if got != want {
			t.Errorf("%s: got %q, want %q", key, got, want)
		}
	}

	// Also verify default shard produces same keys as empty shard
	defaultShardKeys := map[string]string{
		"pending_no_tenant":  QueueKeyPending("GENERATE_MASTER", "", DefaultShardID, 5),
		"pending_tenant":     QueueKeyPending("GENERATE_MASTER", "tenant-123", DefaultShardID, 5),
		"inprog_no_tenant":   QueueKeyInProgress("GENERATE_MASTER", "", DefaultShardID),
		"inprog_tenant":      QueueKeyInProgress("GENERATE_MASTER", "tenant-123", DefaultShardID),
	}

	for key, want := range legacy {
		if got, ok := defaultShardKeys[key]; ok {
			if got != want {
				t.Errorf("default shard %s: got %q, want %q", key, got, want)
			}
		}
	}
}
