package app

import (
	"fmt"
	"log/slog"
	"net"
	"strings"

	"google.golang.org/grpc"

	"github.com/osvaldoandrade/codeq/internal/producer"
	"github.com/osvaldoandrade/codeq/internal/producer/producerpb"
	"github.com/osvaldoandrade/codeq/internal/services"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// startProducerStreamServer mirrors startWorkerStreamServer but for the
// producer-side CreateTask hot path. Phase 3 of the throughput refactor.
func startProducerStreamServer(
	cfg *config.Config,
	scheduler services.SchedulerService,
	producerValidator auth.Validator,
	logger *slog.Logger,
) (*grpcServerHandle, error) {
	addr := strings.TrimSpace(cfg.ProducerStreamAddr)
	if addr == "" {
		return nil, nil
	}
	if producerValidator == nil {
		return nil, fmt.Errorf("producerStreamAddr set but no producer validator configured")
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("producer stream gRPC listen %s: %w", addr, err)
	}

	server := producer.New(scheduler, producerValidator, logger)
	grpcSrv := grpc.NewServer()
	producerpb.RegisterProducerStreamServer(grpcSrv, server)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			logger.Error("producer gRPC server stopped", "err", err)
		}
	}()
	logger.Info("producer streaming gRPC enabled", "addr", addr)

	return &grpcServerHandle{srv: grpcSrv, lis: lis}, nil
}
