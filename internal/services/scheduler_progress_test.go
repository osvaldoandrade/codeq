package services

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const progressNotOwner = "not-owner"

func TestReportProgressPassesThroughToRepository(t *testing.T) {
	ctx, svc := setupSchedulerTest(t)
	if _, err := svc.CreateTask(ctx, domain.CmdGenerateMaster, `{}`, 5, "", 3, "", time.Time{}, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	task, ok, err := svc.ClaimTask(ctx, "worker-1", []domain.Command{domain.CmdGenerateMaster}, 60, 0, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := svc.ReportProgress(ctx, task.ID, "worker-1", json.RawMessage(`{"pct":40}`)); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, err := svc.GetTask(ctx, task.ID)
	if err != nil || string(got.Progress) != `{"pct":40}` {
		t.Fatalf("progress %q err %v", got.Progress, err)
	}
	if err := svc.ReportProgress(ctx, task.ID, "worker-2", json.RawMessage(`1`)); err == nil || err.Error() != progressNotOwner {
		t.Fatalf("other worker: err %v, want %s", err, progressNotOwner)
	}
}
