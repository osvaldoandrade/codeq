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
	BatchSubmit(ctx context.Context, items []domain.BatchSubmitItem) ([]domain.BatchSubmitResponse, error)
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
	var toUpload []domain.ArtifactIn
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
			go func(artifact domain.ArtifactIn) {
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

	if err := s.repo.UpdateTaskOnComplete(taskCtx, taskID, task.Command, task.TenantID, req.Status, req.Error); err != nil {
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

// BatchSubmit optimizes batch result submissions by batching Redis operations
func (s *resultsService) BatchSubmit(ctx context.Context, items []domain.BatchSubmitItem) ([]domain.BatchSubmitResponse, error) {
	if len(items) == 0 {
		return []domain.BatchSubmitResponse{}, nil
	}

	responses := make([]domain.BatchSubmitResponse, len(items))
	taskIDs := make([]string, len(items))

	// Collect all task IDs
	for i, item := range items {
		taskIDs[i] = item.TaskID
	}

	// Batch fetch all tasks in a single pipelined operation (RTT: 1)
	tasks, err := s.repo.GetTasksBatch(ctx, taskIDs)
	if err != nil {
		// On batch fetch error, fall back to individual submissions
		for i, item := range items {
			rec, err := s.Submit(ctx, item.TaskID, item.SubmitResultRequest)
			if err != nil {
				responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: err.Error()}
			} else {
				responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Result: rec}
			}
		}
		return responses, nil
	}

	// Validate all tasks and collect results
	// Use indexMap to track original item index for each valid result
	resultRecords := make([]domain.ResultRecord, 0, len(items))
	taskCompletions := make([]domain.TaskCompleteUpdate, 0, len(items))
	taskDeletes := make([]domain.TaskDeleteInfo, 0, len(items))
	indexMap := make([]int, 0, len(items)) // Maps result index back to item index

	now := s.now().In(s.loc)

	for i, item := range items {
		task, ok := tasks[item.TaskID]
		if !ok {
			responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: "task not found"}
			continue
		}

		req := item.SubmitResultRequest

		// Validate worker ownership
		if task.WorkerID != "" && req.WorkerID != "" && task.WorkerID != req.WorkerID {
			responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: "not-owner"}
			continue
		}

		// Validate task status
		if task.Status != domain.StatusInProgress {
			responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: "not-in-progress"}
			continue
		}

		// Validate result status and fields
		switch req.Status {
		case domain.StatusCompleted:
			if req.Result == nil {
				responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: "result required when status=COMPLETED"}
				continue
			}
		case domain.StatusFailed:
			if strings.TrimSpace(req.Error) == "" {
				responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: "error required when status=FAILED"}
				continue
			}
		default:
			responses[i] = domain.BatchSubmitResponse{TaskID: item.TaskID, Error: "invalid status"}
			continue
		}

		// Create result record
		rec := domain.ResultRecord{
			TaskID:      item.TaskID,
			Status:      req.Status,
			Result:      req.Result,
			Error:       req.Error,
			Artifacts:   []domain.ArtifactOut{},
			CompletedAt: now,
		}

		resultRecords = append(resultRecords, rec)
		taskCompletions = append(taskCompletions, domain.TaskCompleteUpdate{
			ID:       item.TaskID,
			Status:   req.Status,
			ErrorMsg: req.Error,
		})
		taskDeletes = append(taskDeletes, domain.TaskDeleteInfo{
			ID:       item.TaskID,
			Command:  task.Command,
			TenantID: task.TenantID,
		})
		indexMap = append(indexMap, i) // Track original index
	}

	// Batch save all results (RTT: 1 for all results)
	for i, rec := range resultRecords {
		if err := s.repo.SaveResult(ctx, rec); err != nil {
			responses[indexMap[i]] = domain.BatchSubmitResponse{TaskID: items[indexMap[i]].TaskID, Error: err.Error()}
		}
	}

	// Batch update tasks (RTT: 1 for all task updates)
	if len(taskCompletions) > 0 {
		if err := s.repo.BatchUpdateTasksOnComplete(ctx, taskCompletions); err != nil {
			// Mark all as failed
			for i := range taskCompletions {
				responses[indexMap[i]].Error = err.Error()
				responses[indexMap[i]].Result = nil
			}
			return responses, err
		}
	}

	// inprog removal + lease deletion are part of BatchUpdateTasksOnComplete's
	// TxPipeline (kept atomic to avoid resurrection by the claim-time requeue probe).

	// Populate successful responses
	for i := range resultRecords {
		origIdx := indexMap[i]
		if responses[origIdx].Error == "" {
			responses[origIdx] = domain.BatchSubmitResponse{
				TaskID: items[origIdx].TaskID,
				Result: &resultRecords[i],
			}
		}
	}

	return responses, nil
}
