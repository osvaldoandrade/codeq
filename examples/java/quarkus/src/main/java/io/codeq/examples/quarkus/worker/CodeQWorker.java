package io.codeq.examples.quarkus.worker;

import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.CodeQException;
import io.codeq.sdk.Task;
import io.quarkus.runtime.ShutdownEvent;
import io.quarkus.scheduler.Scheduled;
import jakarta.enterprise.context.ApplicationScoped;
import jakarta.enterprise.event.Observes;
import jakarta.inject.Inject;
import org.jboss.logging.Logger;

import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/**
 * Worker that polls and processes tasks from CodeQ.
 * 
 * Features:
 * - Polls for tasks every 5 seconds
 * - Claims tasks with 120s lease
 * - Sends heartbeats every 30s
 * - Processes tasks and submits results
 * - Handles errors with NACK
 */
@ApplicationScoped
public class CodeQWorker {

    @Inject
    Logger log;

    @Inject
    CodeQClient codeQClient;

    private Task currentTask;

    /**
     * Polls for tasks and processes them.
     * Runs every 5 seconds.
     */
    @Scheduled(every = "5s")
    void pollAndProcess() {
        if (currentTask != null) {
            log.debugf("Already processing task: %s", currentTask.getId());
            return;
        }

        try {
            Task task = codeQClient.claimTask(
                List.of("GENERATE_MASTER", "GENERATE_CREATIVE"),
                120,
                10
            );

            if (task == null) {
                log.debug("No tasks available");
                return;
            }

            currentTask = task;
            log.infof("Claimed task: %s (command: %s)", task.getId(), task.getCommand());

            processTask(task);

        } catch (CodeQException e) {
            log.error("Error claiming task", e);
        }
    }

    /**
     * Sends heartbeat for current task.
     * Runs every 30 seconds.
     */
    @Scheduled(every = "30s")
    void sendHeartbeat() {
        if (currentTask == null) {
            return;
        }

        try {
            codeQClient.heartbeat(currentTask.getId(), 60);
            log.debugf("Heartbeat sent for task: %s", currentTask.getId());
        } catch (CodeQException e) {
            log.errorf("Error sending heartbeat for task: %s", currentTask.getId(), e);
        }
    }

    /**
     * Processes a task based on its command type.
     */
    private void processTask(Task task) {
        try {
            log.infof("Processing task: %s with payload: %s", task.getId(), task.getPayload());

            Map<String, Object> result = switch (task.getCommand()) {
                case "GENERATE_MASTER" -> processMasterGeneration(task);
                case "GENERATE_CREATIVE" -> processCreativeGeneration(task);
                default -> throw new IllegalArgumentException("Unknown command: " + task.getCommand());
            };

            codeQClient.submitResult(task.getId(), result);
            log.infof("Task completed successfully: %s", task.getId());

        } catch (Exception e) {
            log.errorf("Error processing task: %s", task.getId(), e);
            handleTaskError(task, e);
        } finally {
            currentTask = null;
        }
    }

    /**
     * Processes GENERATE_MASTER command.
     */
    private Map<String, Object> processMasterGeneration(Task task) throws InterruptedException {
        String jobId = (String) task.getPayload().get("jobId");
        log.infof("Generating master for jobId: %s", jobId);

        TimeUnit.SECONDS.sleep(5);

        return Map.of(
            "status", "success",
            "jobId", jobId,
            "masterUrl", "https://storage.example.com/masters/" + jobId + ".mp4",
            "duration", 120.5,
            "processedAt", System.currentTimeMillis()
        );
    }

    /**
     * Processes GENERATE_CREATIVE command.
     */
    private Map<String, Object> processCreativeGeneration(Task task) throws InterruptedException {
        log.info("Generating creative content");

        TimeUnit.SECONDS.sleep(3);

        return Map.of(
            "status", "success",
            "creativeUrl", "https://storage.example.com/creatives/creative-123.jpg",
            "processedAt", System.currentTimeMillis()
        );
    }

    /**
     * Handles task processing errors.
     */
    private void handleTaskError(Task task, Exception error) {
        try {
            if (isRetryable(error)) {
                int delaySeconds = calculateBackoff(task.getAttempts());
                codeQClient.nack(task.getId(), delaySeconds, error.getMessage());
                log.warnf("Task NACKed for retry: %s (delay: %ds)", task.getId(), delaySeconds);
            } else {
                codeQClient.submitResult(
                    task.getId(),
                    "FAILED",
                    Map.of("error", error.getMessage()),
                    error.getMessage()
                );
                log.errorf("Task failed permanently: %s", task.getId());
            }
        } catch (CodeQException e) {
            log.errorf("Error handling task failure: %s", task.getId(), e);
        }
    }

    /**
     * Determines if an error is retryable.
     */
    private boolean isRetryable(Exception error) {
        return !(error instanceof IllegalArgumentException);
    }

    /**
     * Calculates backoff delay based on attempt count.
     */
    private int calculateBackoff(Integer attempts) {
        int attempt = attempts != null ? attempts : 0;
        return Math.min(300, (int) Math.pow(2, attempt) * 5);
    }

    /**
     * Handles graceful shutdown.
     */
    void onShutdown(@Observes ShutdownEvent event) {
        if (currentTask != null) {
            try {
                codeQClient.abandon(currentTask.getId());
                log.infof("Abandoned task on shutdown: %s", currentTask.getId());
            } catch (CodeQException e) {
                log.error("Failed to abandon task on shutdown", e);
            }
        }
    }
}
