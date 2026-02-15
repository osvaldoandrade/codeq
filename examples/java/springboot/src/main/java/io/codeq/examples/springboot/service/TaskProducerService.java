package io.codeq.examples.springboot.service;

import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.CodeQException;
import io.codeq.sdk.Task;
import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.stereotype.Service;

import java.util.Map;

/**
 * Service for producing tasks to CodeQ.
 * 
 * This service wraps the CodeQ client and provides
 * business logic for task creation.
 */
@Service
@RequiredArgsConstructor
@Slf4j
public class TaskProducerService {

    private final CodeQClient codeQClient;

    /**
     * Creates a task for generating master content.
     * 
     * @param jobId Job identifier
     * @param priority Task priority (0-9)
     * @return Created task
     * @throws CodeQException if task creation fails
     */
    public Task createMasterGenerationTask(String jobId, int priority) throws CodeQException {
        log.info("Creating GENERATE_MASTER task for jobId: {}", jobId);
        
        Map<String, Object> payload = Map.of(
            "jobId", jobId,
            "timestamp", System.currentTimeMillis()
        );

        Task task = codeQClient.createTask("GENERATE_MASTER", payload, priority);
        
        log.info("Task created successfully: {}", task.getId());
        return task;
    }

    /**
     * Creates a task with webhook callback.
     * 
     * @param command Command type
     * @param payload Task payload
     * @param webhookUrl Webhook URL for result callback
     * @return Created task
     * @throws CodeQException if task creation fails
     */
    public Task createTaskWithWebhook(String command, Map<String, Object> payload, String webhookUrl) throws CodeQException {
        log.info("Creating {} task with webhook: {}", command, webhookUrl);
        
        Task task = codeQClient.createTask(command, payload, 5, webhookUrl, 3, null, null);
        
        log.info("Task created with webhook: {}", task.getId());
        return task;
    }

    /**
     * Creates a delayed task.
     * 
     * @param command Command type
     * @param payload Task payload
     * @param delaySeconds Delay in seconds
     * @return Created task
     * @throws CodeQException if task creation fails
     */
    public Task createDelayedTask(String command, Map<String, Object> payload, int delaySeconds) throws CodeQException {
        log.info("Creating delayed {} task (delay: {}s)", command, delaySeconds);
        
        Task task = codeQClient.createTask(command, payload, 5, null, null, delaySeconds, null);
        
        log.info("Delayed task created: {}", task.getId());
        return task;
    }
}
