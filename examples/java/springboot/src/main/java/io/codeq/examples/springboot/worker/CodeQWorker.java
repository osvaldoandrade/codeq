package io.codeq.examples.springboot.worker;

import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.CodeQException;
import io.codeq.sdk.Task;
import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Component;

import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/**
 * Worker that polls and processes tasks from CodeQ.
 * 
 * This worker:
 * - Polls for tasks every 5 seconds
 * - Claims tasks with 120s lease
 * - Sends heartbeats every 30s
 * - Processes tasks and submits results
 * - Handles errors with NACK
 */
@Component
@RequiredArgsConstructor
@Slf4j
public class CodeQWorker {

    private final CodeQClient codeQClient;
    private Task currentTask;

    /**
     * Polls for tasks and processes them.
     * Runs every 5 seconds.
     */
    @Scheduled(fixedDelay = 5000)
    public void pollAndProcess() {
        if (currentTask != null) {
            log.debug("Already processing task: {}", currentTask.getId());
            return;
        }

        try {
            // Claim a task with long-polling (wait up to 10 seconds)
            Task task = codeQClient.claimTask(
                List.of("GENERATE_MASTER", "GENERATE_CREATIVE"),
                120, // 120s lease
                10   // 10s long-poll
            );

            if (task == null) {
                log.debug("No tasks available");
                return;
            }

            currentTask = task;
            log.info("Claimed task: {} (command: {})", task.getId(), task.getCommand());

            // Process the task
            processTask(task);

        } catch (CodeQException e) {
            log.error("Error claiming task", e);
        }
    }

    /**
     * Sends heartbeat for current task.
     * Runs every 30 seconds.
     */
    @Scheduled(fixedDelay = 30000)
    public void sendHeartbeat() {
        if (currentTask == null) {
            return;
        }

        try {
            codeQClient.heartbeat(currentTask.getId(), 60);
            log.debug("Heartbeat sent for task: {}", currentTask.getId());
        } catch (CodeQException e) {
            log.error("Error sending heartbeat for task: {}", currentTask.getId(), e);
        }
    }

    /**
     * Processes a task based on its command type.
     * 
     * @param task Task to process
     */
    private void processTask(Task task) {
        try {
            log.info("Processing task: {} with payload: {}", task.getId(), task.getPayload());

            // Simulate processing based on command
            Map<String, Object> result = switch (task.getCommand()) {
                case "GENERATE_MASTER" -> processMasterGeneration(task);
                case "GENERATE_CREATIVE" -> processCreativeGeneration(task);
                default -> throw new IllegalArgumentException("Unknown command: " + task.getCommand());
            };

            // Submit successful result
            codeQClient.submitResult(task.getId(), result);
            log.info("Task completed successfully: {}", task.getId());

        } catch (Exception e) {
            log.error("Error processing task: {}", task.getId(), e);
            handleTaskError(task, e);
        } finally {
            currentTask = null;
        }
    }

    /**
     * Processes GENERATE_MASTER command.
     * 
     * @param task Task to process
     * @return Result data
     */
    private Map<String, Object> processMasterGeneration(Task task) throws InterruptedException {
        String jobId = (String) task.getPayload().get("jobId");
        log.info("Generating master for jobId: {}", jobId);

        // Simulate work
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
     * 
     * @param task Task to process
     * @return Result data
     */
    private Map<String, Object> processCreativeGeneration(Task task) throws InterruptedException {
        log.info("Generating creative content");

        // Simulate work
        TimeUnit.SECONDS.sleep(3);

        return Map.of(
            "status", "success",
            "creativeUrl", "https://storage.example.com/creatives/creative-123.jpg",
            "processedAt", System.currentTimeMillis()
        );
    }

    /**
     * Handles task processing errors.
     * 
     * @param task Failed task
     * @param error Error that occurred
     */
    private void handleTaskError(Task task, Exception error) {
        try {
            // Check if error is retryable
            if (isRetryable(error)) {
                // NACK with exponential backoff
                int delaySeconds = calculateBackoff(task.getAttempts());
                codeQClient.nack(task.getId(), delaySeconds, error.getMessage());
                log.warn("Task NACKed for retry: {} (delay: {}s)", task.getId(), delaySeconds);
            } else {
                // Submit failed result
                codeQClient.submitResult(
                    task.getId(),
                    "FAILED",
                    Map.of("error", error.getMessage()),
                    error.getMessage()
                );
                log.error("Task failed permanently: {}", task.getId());
            }
        } catch (CodeQException e) {
            log.error("Error handling task failure: {}", task.getId(), e);
        }
    }

    /**
     * Determines if an error is retryable.
     * 
     * @param error Error to check
     * @return true if retryable
     */
    private boolean isRetryable(Exception error) {
        // Retry on transient errors, not on business logic errors
        return !(error instanceof IllegalArgumentException);
    }

    /**
     * Calculates backoff delay based on attempt count.
     * 
     * @param attempts Number of attempts
     * @return Delay in seconds
     */
    private int calculateBackoff(Integer attempts) {
        int attempt = attempts != null ? attempts : 0;
        return Math.min(300, (int) Math.pow(2, attempt) * 5); // Max 5 minutes
    }
}
