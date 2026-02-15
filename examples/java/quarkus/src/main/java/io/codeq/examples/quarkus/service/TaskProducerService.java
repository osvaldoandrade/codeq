package io.codeq.examples.quarkus.service;

import io.codeq.sdk.CodeQClient;
import io.codeq.sdk.CodeQException;
import io.codeq.sdk.Task;
import jakarta.enterprise.context.ApplicationScoped;
import jakarta.inject.Inject;
import org.jboss.logging.Logger;

import java.util.Map;

/**
 * Service for producing tasks to CodeQ.
 */
@ApplicationScoped
public class TaskProducerService {

    @Inject
    Logger log;

    @Inject
    CodeQClient codeQClient;

    /**
     * Creates a master generation task.
     * 
     * @param jobId Job identifier
     * @param priority Task priority (0-9)
     * @return Created task
     * @throws CodeQException if task creation fails
     */
    public Task createMasterTask(String jobId, int priority) throws CodeQException {
        log.infof("Creating GENERATE_MASTER task for jobId: %s", jobId);
        
        Map<String, Object> payload = Map.of(
            "jobId", jobId,
            "timestamp", System.currentTimeMillis()
        );

        Task task = codeQClient.createTask("GENERATE_MASTER", payload, priority);
        
        log.infof("Task created: %s", task.getId());
        return task;
    }

    /**
     * Creates a task with webhook callback.
     * 
     * @param command Command type
     * @param payload Task payload
     * @param webhookUrl Webhook URL
     * @return Created task
     * @throws CodeQException if task creation fails
     */
    public Task createTaskWithWebhook(String command, Map<String, Object> payload, String webhookUrl) throws CodeQException {
        log.infof("Creating %s task with webhook: %s", command, webhookUrl);
        
        Task task = codeQClient.createTask(command, payload, 5, webhookUrl, 3, null, null);
        
        log.infof("Task created with webhook: %s", task.getId());
        return task;
    }
}
