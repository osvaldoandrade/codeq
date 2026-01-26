package services

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/internal/providers"
	"github.com/osvaldoandrade/codeq/internal/repository"
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
	if task.WorkerID != "" && req.WorkerID != "" && task.WorkerID != req.WorkerID {
		return nil, fmt.Errorf("not-owner")
	}
	if task.Status != domain.StatusInProgress {
		return nil, fmt.Errorf("not-in-progress")
	}

	var outs []domain.ArtifactOut
	for _, a := range req.Artifacts {
		if a.URL != "" {
			outs = append(outs, domain.ArtifactOut{Name: a.Name, URL: a.URL})
			continue
		}
		if a.ContentBase64 == "" {
			continue
		}
		data, err := s.repo.DecodeBase64(a.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("artifact %s base64 decode: %w", a.Name, err)
		}
		objPath := path.Join("results", taskID, a.Name)
		url, err := s.uploader.UploadBytes(ctx, objPath, a.ContentType, data)
		if err != nil {
			return nil, fmt.Errorf("artifact %s upload: %w", a.Name, err)
		}
		outs = append(outs, domain.ArtifactOut{Name: a.Name, URL: url})
	}

	switch req.Status {
	case domain.StatusCompleted:
		if req.Result == nil {
			return nil, fmt.Errorf("result required when status=COMPLETED")
		}
	case domain.StatusFailed:
		if strings.TrimSpace(req.Error) == "" {
			return nil, fmt.Errorf("error required when status=FAILED")
		}
	default:
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
	if err := s.repo.SaveResult(ctx, rec); err != nil {
		return nil, err
	}

	if err := s.repo.UpdateTaskOnComplete(ctx, taskID, req.Status, req.Error); err != nil {
		return nil, err
	}
	if err := s.repo.RemoveFromInprogAndClearLease(ctx, taskID, task.Command); err != nil {
		return nil, err
	}

	if s.callback != nil {
		s.callback.Send(context.Background(), *task, rec)
	}

	return &rec, nil
}

func (s *resultsService) Get(ctx context.Context, taskID string) (*domain.ResultRecord, *domain.Task, error) {
	task, err := s.repo.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("task not found")
	}
	res, err := s.repo.GetResult(ctx, taskID)
	if err != nil {
		return nil, task, fmt.Errorf("result not found")
	}
	return res, task, nil
}
