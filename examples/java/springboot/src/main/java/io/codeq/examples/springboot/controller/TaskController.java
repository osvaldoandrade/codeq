package io.codeq.examples.springboot.controller;

import io.codeq.sdk.CodeQException;
import io.codeq.sdk.Task;
import io.codeq.examples.springboot.service.TaskProducerService;
import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.util.Map;

/**
 * REST controller for creating tasks.
 * 
 * Exposes endpoints for external systems to enqueue tasks.
 */
@RestController
@RequestMapping("/api/tasks")
@RequiredArgsConstructor
@Slf4j
public class TaskController {

    private final TaskProducerService producerService;

    /**
     * Creates a master generation task.
     * 
     * POST /api/tasks/master
     * Body: { "jobId": "123", "priority": 5 }
     * 
     * @param request Request body
     * @return Created task
     */
    @PostMapping("/master")
    public ResponseEntity<Task> createMasterTask(@RequestBody Map<String, Object> request) {
        try {
            String jobId = (String) request.get("jobId");
            int priority = (int) request.getOrDefault("priority", 5);

            Task task = producerService.createMasterGenerationTask(jobId, priority);
            return ResponseEntity.ok(task);

        } catch (CodeQException e) {
            log.error("Failed to create master task", e);
            return ResponseEntity.internalServerError().build();
        }
    }

    /**
     * Creates a task with webhook.
     * 
     * POST /api/tasks/with-webhook
     * Body: { "command": "GENERATE_MASTER", "payload": {...}, "webhook": "https://..." }
     * 
     * @param request Request body
     * @return Created task
     */
    @PostMapping("/with-webhook")
    public ResponseEntity<Task> createTaskWithWebhook(@RequestBody Map<String, Object> request) {
        try {
            String command = (String) request.get("command");
            Map<String, Object> payload = (Map<String, Object>) request.get("payload");
            String webhook = (String) request.get("webhook");

            Task task = producerService.createTaskWithWebhook(command, payload, webhook);
            return ResponseEntity.ok(task);

        } catch (CodeQException e) {
            log.error("Failed to create task with webhook", e);
            return ResponseEntity.internalServerError().build();
        }
    }

    /**
     * Creates a delayed task.
     * 
     * POST /api/tasks/delayed
     * Body: { "command": "GENERATE_MASTER", "payload": {...}, "delaySeconds": 60 }
     * 
     * @param request Request body
     * @return Created task
     */
    @PostMapping("/delayed")
    public ResponseEntity<Task> createDelayedTask(@RequestBody Map<String, Object> request) {
        try {
            String command = (String) request.get("command");
            Map<String, Object> payload = (Map<String, Object>) request.get("payload");
            int delaySeconds = (int) request.getOrDefault("delaySeconds", 0);

            Task task = producerService.createDelayedTask(command, payload, delaySeconds);
            return ResponseEntity.ok(task);

        } catch (CodeQException e) {
            log.error("Failed to create delayed task", e);
            return ResponseEntity.internalServerError().build();
        }
    }
}
