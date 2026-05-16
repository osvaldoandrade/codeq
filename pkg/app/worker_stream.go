package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"google.golang.org/grpc"

	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/internal/worker"
	"github.com/osvaldoandrade/codeq/internal/worker/workerpb"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

type grpcServerHandle struct {
	srv *grpc.Server
	lis net.Listener
}

func startWorkerStreamServer(
	cfg *config.Config,
	scheduler services.SchedulerService,
	results services.ResultsService,
	workerValidator auth.Validator,
	producerValidator auth.Validator,
	logger *slog.Logger,
) (*grpcServerHandle, error) {
	addr := strings.TrimSpace(cfg.WorkerStreamAddr)
	if addr == "" {
		return nil, nil
	}
	if workerValidator == nil {
		return nil, fmt.Errorf("workerStreamAddr set but no worker validator configured")
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("worker stream gRPC listen %s: %w", addr, err)
	}

	server := worker.New(
		scheduler,
		results,
		workerValidator,
		logger,
		cfg.DefaultLeaseSeconds,
		cfg.WorkerAudience,
	)
	server.ProducerValidator = producerValidator
	server.AllowProducerAsWorker = cfg.AllowProducerAsWorker

	grpcSrv := grpc.NewServer()
	workerpb.RegisterWorkerStreamServer(grpcSrv, server)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			logger.Error("worker gRPC server stopped", "err", err)
		}
	}()
	logger.Info("worker streaming gRPC enabled", "addr", addr)

	return &grpcServerHandle{srv: grpcSrv, lis: lis}, nil
}

func stopGRPCServer(ctx context.Context, handle *grpcServerHandle) {
	if handle == nil {
		return
	}
	if handle.srv != nil {
		done := make(chan struct{})
		go func() {
			handle.srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			handle.srv.Stop()
		}
	}
	if handle.lis != nil {
		_ = handle.lis.Close()
	}
}
