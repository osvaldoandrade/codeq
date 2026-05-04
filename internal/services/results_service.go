package services

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/providers"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/internal/tracing"
	"github.com/osvaldoandrade/codeq/pkg/domain"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type ResultsService interface {
	Submit(ctx context.Context, taskID string, req domain.SubmitResultRequest) (*domain.ResultRecord, error)
	Get(ctx context.Context, taskID string) (*domain.ResultRecord, *domain.Task, error)
}

type resultsService struct {
	repo     repository.ResultRepository
	uploader providers.Uploader
	callback ResultCallbackService
	logger   *slog.Logger
	now      func() time.Time
	loc      *time.Location
}

func NewResultsService(repo repository.ResultRepository, uploader providers.Uploader, callback ResultCallbackService, logger *slog.Logger, now func() time.Time, loc *time.Location) ResultsService {
	return &resultsService{repo: repo, uploader: uploader, callback: callback, logger: logger, now: now, loc: loc}
}

func (s *resultsService) Submit(ctx context.Context, taskID string, req domain.SubmitResultRequest) (*domain.ResultRecord, error) {
	task, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task not found")
	}

	taskCtx := tracing.ContextWithRemoteParent(ctx, task.TraceParent, task.TraceState)
	taskCtx, span := otel.Tracer("codeq/results").Start(taskCtx, "codeq.task.submit_result",
		trace.WithAttributes(
			attribute.String("codeq.task_id", taskID),
			attribute.String("codeq.command", string(task.Command)),
			attribute.String("codeq.tenant_id", task.TenantID),
			attribute.String("codeq.submit.status", string(req.Status)),
		),
	)
	defer span.End()

	if task.WorkerID != "" && req.WorkerID != "" && task.WorkerID != req.WorkerID {
		span.SetStatus(codes.Error, "not-owner")
		return nil, fmt.Errorf("not-owner")
	}
	if task.Status != domain.StatusInProgress {
		span.SetStatus(codes.Error, "not-in-progress")
		return nil, fmt.Errorf("not-in-progress")
	}

	var outs []domain.ArtifactOut
	var outsMu sync.Mutex

	// First pass: collect artifacts to upload and existing artifacts with URLs
	var toUpload []domain.SubmitArtifact
	for _, a := range req.Artifacts {
		if a.URL != "" {
			outsMu.Lock()
			outs = append(outs, domain.ArtifactOut{Name: a.Name, URL: a.URL})
			outsMu.Unlock()
			continue
		}
		if a.ContentBase64 == "" {
			continue
		}
		toUpload = append(toUpload, a)
	}

	// Second pass: decode and upload artifacts concurrently (max 5 concurrent uploads)
	if len(toUpload) > 0 {
		sem := make(chan struct{}, 5)
		var wg sync.WaitGroup
		var uploadErr error
		var errMu sync.Mutex

		for _, a := range toUpload {
			wg.Add(1)
			go func(artifact domain.SubmitArtifact) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				if uploadErr != nil {
					return
				}

				data, err := s.repo.DecodeBase64(artifact.ContentBase64)
				if err != nil {
					errMu.Lock()
					if uploadErr == nil {
						span.RecordError(err)
						uploadErr = fmt.Errorf("artifact %s base64 decode: %w", artifact.Name, err)
					}
					errMu.Unlock()
					return
				}

				objPath := path.Join("results", taskID, artifact.Name)
				url, err := s.uploader.UploadBytes(taskCtx, objPath, artifact.ContentType, data)
				if err != nil {
					errMu.Lock()
					if uploadErr == nil {
						span.RecordError(err)
						uploadErr = fmt.Errorf("artifact %s upload: %w", artifact.Name, err)
					}
					errMu.Unlock()
					return
				}

				outsMu.Lock()
				outs = append(outs, domain.ArtifactOut{Name: artifact.Name, URL: url})
				outsMu.Unlock()
			}(a)
		}

		wg.Wait()
		if uploadErr != nil {
			span.SetStatus(codes.Error, uploadErr.Error())
			return nil, uploadErr
		}
	}

	switch req.Status {
	case domain.StatusCompleted:
		if req.Result == nil {
			span.SetStatus(codes.Error, "result required")
			return nil, fmt.Errorf("result required when status=COMPLETED")
		}
	case domain.StatusFailed:
		if strings.TrimSpace(req.Error) == "" {
			span.SetStatus(codes.Error, "error required")
			return nil, fmt.Errorf("error required when status=FAILED")
		}
	default:
		span.SetStatus(codes.Error, "invalid status")
		return nil, fmt.Errorf("invalid status")
	}

	rec := domain.ResultRecord{
		TaskID:      taskID,
		Status:      req.Status,
		Result:      req.Result,
		Error:       req.Error,
		Artifacts:   outs,
		CompletedAt: s.now().In(s.loc),
	}
	if err := s.repo.SaveResult(taskCtx, rec); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	if err := s.repo.UpdateTaskOnComplete(taskCtx, taskID, req.Status, req.Error); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if err := s.repo.RemoveFromInprogAndClearLease(taskCtx, taskID, task.Command); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	metrics.TaskCompletedTotal.WithLabelValues(string(task.Command), string(req.Status)).Inc()
	if d := rec.CompletedAt.Sub(task.CreatedAt).Seconds(); d >= 0 {
		metrics.TaskProcessingLatencySeconds.WithLabelValues(string(task.Command), string(req.Status)).Observe(d)
	}

	if s.callback != nil {
		s.callback.Send(context.WithoutCancel(taskCtx), *task, rec)
	}

	return &rec, nil
}

func (s *resultsService) Get(ctx context.Context, taskID string) (*domain.ResultRecord, *domain.Task, error) {
	task, res, err := s.repo.GetTaskAndResult(ctx, taskID)
	if err != nil {
		// Handle case where task exists but result doesn't
		if task != nil {
			return nil, task, fmt.Errorf("result not found")
		}
		return nil, nil, fmt.Errorf("task not found")
	}
	return res, task, nil
}
