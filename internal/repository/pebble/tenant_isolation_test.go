package pebble

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

func TestTenantQueuesStaySeparate(t *testing.T) {
	ctx := context.Background()
	repo := NewTaskRepository(openTestDB(t), time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	if _, err := repo.Enqueue(ctx, cmd, `{"n":1}`, 0, "", 5, "", "", "", time.Time{}, "tenant-a"); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	if _, err := repo.Enqueue(ctx, cmd, `{"n":2}`, 0, "", 5, "", "", "", time.Time{}, "tenant-b"); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}

	gotA, ok, err := repo.Claim(ctx, "worker-a", []domain.Command{cmd}, 30, 10, 5, "tenant-a")
	if err != nil || !ok {
		t.Fatalf("claim a: ok=%v err=%v", ok, err)
	}
	if gotA.TenantID != "tenant-a" {
		t.Fatalf("worker A claimed %s", gotA.TenantID)
	}

	gotB, ok, err := repo.Claim(ctx, "worker-b", []domain.Command{cmd}, 30, 10, 5, "tenant-b")
	if err != nil || !ok {
		t.Fatalf("claim b: ok=%v err=%v", ok, err)
	}
	if gotB.TenantID != "tenant-b" {
		t.Fatalf("worker B claimed %s", gotB.TenantID)
	}

	if _, ok, err := repo.Claim(ctx, "worker-a", []domain.Command{cmd}, 30, 10, 5, "tenant-a"); err != nil || ok {
		t.Fatalf("tenant A queue should be empty: ok=%v err=%v", ok, err)
	}
}
