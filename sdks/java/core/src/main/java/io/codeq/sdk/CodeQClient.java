package io.codeq.sdk;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import okhttp3.*;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

import java.io.IOException;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/**
 * CodeQ Client for producing and consuming tasks.
 * 
 * This client provides methods to:
 * - Create tasks (producer role)
 * - Claim tasks (worker role)
 * - Submit results
 * - Manage task lifecycle (heartbeat, abandon, nack)
 * 
 * Example usage:
 * <pre>
 * CodeQClient client = CodeQClient.builder()
 *     .baseUrl("https://codeq.example.com")
 *     .producerToken("your-producer-token")
 *     .workerToken("your-worker-token")
 *     .build();
 * 
 * // Create a task
 * Task task = client.createTask("GENERATE_MASTER", Map.of("jobId", "123"), 5);
 * 
 * // Claim a task
 * Task claimed = client.claimTask(List.of("GENERATE_MASTER"), 120, 10);
 * 
 * // Submit result
 * client.submitResult(claimed.getId(), Map.of("status", "success"));
 * </pre>
 */
public class CodeQClient {
    
    private static final Logger log = LoggerFactory.getLogger(CodeQClient.class);
    private static final MediaType JSON = MediaType.get("application/json; charset=utf-8");
    
    private final String baseUrl;
    private final String producerToken;
    private final String workerToken;
    private final OkHttpClient httpClient;
    private final ObjectMapper objectMapper;

    private CodeQClient(Builder builder) {
        this.baseUrl = builder.baseUrl.endsWith("/") 
            ? builder.baseUrl.substring(0, builder.baseUrl.length() - 1) 
            : builder.baseUrl;
        this.producerToken = builder.producerToken;
        this.workerToken = builder.workerToken;
        this.httpClient = builder.httpClient != null ? builder.httpClient : createDefaultHttpClient();
        this.objectMapper = new ObjectMapper().registerModule(new JavaTimeModule());
    }

    /**
     * Creates a new task in the queue.
     * 
     * @param command The command type (e.g., "GENERATE_MASTER")
     * @param payload Task payload as a map
     * @param priority Task priority (0-9, higher is more urgent)
     * @return Created task with ID
     * @throws CodeQException if task creation fails
     */
    public Task createTask(String command, Map<String, Object> payload, int priority) throws CodeQException {
        return createTask(command, payload, priority, null, null, null, null);
    }

    /**
     * Creates a new task with full options.
     * 
     * @param command The command type
     * @param payload Task payload
     * @param priority Task priority (0-9)
     * @param webhook Optional webhook URL for result callback
     * @param maxAttempts Maximum retry attempts
     * @param delaySeconds Delay before task becomes available
     * @param idempotencyKey Optional idempotency key
     * @return Created task
     * @throws CodeQException if task creation fails
     */
    public Task createTask(String command, Map<String, Object> payload, int priority, 
                          String webhook, Integer maxAttempts, Integer delaySeconds, 
                          String idempotencyKey) throws CodeQException {
        try {
            Map<String, Object> body = Map.of(
                "command", command,
                "payload", payload,
                "priority", priority
            );
            
            // Add optional fields
            if (webhook != null) ((Map<String, Object>) body).put("webhook", webhook);
            if (maxAttempts != null) ((Map<String, Object>) body).put("maxAttempts", maxAttempts);
            if (delaySeconds != null) ((Map<String, Object>) body).put("delaySeconds", delaySeconds);
            if (idempotencyKey != null) ((Map<String, Object>) body).put("idempotencyKey", idempotencyKey);

            String json = objectMapper.writeValueAsString(body);
            
            Request request = new Request.Builder()
                .url(baseUrl + "/v1/codeq/tasks")
                .header("Authorization", "Bearer " + producerToken)
                .header("Content-Type", "application/json")
                .post(RequestBody.create(json, JSON))
                .build();

            try (Response response = httpClient.newCall(request).execute()) {
                if (!response.isSuccessful()) {
                    throw new CodeQException("Failed to create task: " + response.code() + " " + response.message());
                }
                return objectMapper.readValue(response.body().string(), Task.class);
            }
        } catch (IOException e) {
            throw new CodeQException("Error creating task", e);
        }
    }

    /**
     * Claims a task from the queue (worker operation).
     * 
     * @param commands List of command types to claim
     * @param leaseSeconds Lease duration in seconds
     * @param waitSeconds Long-polling wait time (0-30 seconds)
     * @return Claimed task or null if no task available
     * @throws CodeQException if claim fails
     */
    public Task claimTask(List<String> commands, int leaseSeconds, int waitSeconds) throws CodeQException {
        try {
            Map<String, Object> body = Map.of(
                "commands", commands,
                "leaseSeconds", leaseSeconds,
                "waitSeconds", waitSeconds
            );

            String json = objectMapper.writeValueAsString(body);
            
            Request request = new Request.Builder()
                .url(baseUrl + "/v1/codeq/tasks/claim")
                .header("Authorization", "Bearer " + workerToken)
                .header("Content-Type", "application/json")
                .post(RequestBody.create(json, JSON))
                .build();

            try (Response response = httpClient.newCall(request).execute()) {
                if (response.code() == 204) {
                    return null; // No task available
                }
                if (!response.isSuccessful()) {
                    throw new CodeQException("Failed to claim task: " + response.code() + " " + response.message());
                }
                return objectMapper.readValue(response.body().string(), Task.class);
            }
        } catch (IOException e) {
            throw new CodeQException("Error claiming task", e);
        }
    }

    /**
     * Submits a result for a completed task.
     * 
     * @param taskId Task ID
     * @param result Result data
     * @throws CodeQException if submission fails
     */
    public void submitResult(String taskId, Map<String, Object> result) throws CodeQException {
        submitResult(taskId, "COMPLETED", result, null);
    }

    /**
     * Submits a result with status.
     * 
     * @param taskId Task ID
     * @param status Result status (COMPLETED or FAILED)
     * @param result Result data
     * @param error Optional error message
     * @throws CodeQException if submission fails
     */
    public void submitResult(String taskId, String status, Map<String, Object> result, String error) throws CodeQException {
        try {
            Map<String, Object> body = Map.of(
                "status", status,
                "result", result
            );
            if (error != null) ((Map<String, Object>) body).put("error", error);

            String json = objectMapper.writeValueAsString(body);
            
            Request request = new Request.Builder()
                .url(baseUrl + "/v1/codeq/tasks/" + taskId + "/result")
                .header("Authorization", "Bearer " + workerToken)
                .header("Content-Type", "application/json")
                .post(RequestBody.create(json, JSON))
                .build();

            try (Response response = httpClient.newCall(request).execute()) {
                if (!response.isSuccessful()) {
                    throw new CodeQException("Failed to submit result: " + response.code() + " " + response.message());
                }
            }
        } catch (IOException e) {
            throw new CodeQException("Error submitting result", e);
        }
    }

    /**
     * Extends the lease on a task (heartbeat).
     * 
     * @param taskId Task ID
     * @param extendSeconds Seconds to extend lease
     * @throws CodeQException if heartbeat fails
     */
    public void heartbeat(String taskId, int extendSeconds) throws CodeQException {
        try {
            Map<String, Object> body = Map.of("extendSeconds", extendSeconds);
            String json = objectMapper.writeValueAsString(body);
            
            Request request = new Request.Builder()
                .url(baseUrl + "/v1/codeq/tasks/" + taskId + "/heartbeat")
                .header("Authorization", "Bearer " + workerToken)
                .header("Content-Type", "application/json")
                .post(RequestBody.create(json, JSON))
                .build();

            try (Response response = httpClient.newCall(request).execute()) {
                if (!response.isSuccessful()) {
                    throw new CodeQException("Failed to heartbeat: " + response.code() + " " + response.message());
                }
            }
        } catch (IOException e) {
            throw new CodeQException("Error sending heartbeat", e);
        }
    }

    /**
     * Abandons a task (returns it to the queue).
     * 
     * @param taskId Task ID
     * @throws CodeQException if abandon fails
     */
    public void abandon(String taskId) throws CodeQException {
        try {
            Request request = new Request.Builder()
                .url(baseUrl + "/v1/codeq/tasks/" + taskId + "/abandon")
                .header("Authorization", "Bearer " + workerToken)
                .post(RequestBody.create("{}", JSON))
                .build();

            try (Response response = httpClient.newCall(request).execute()) {
                if (!response.isSuccessful()) {
                    throw new CodeQException("Failed to abandon task: " + response.code() + " " + response.message());
                }
            }
        } catch (IOException e) {
            throw new CodeQException("Error abandoning task", e);
        }
    }

    /**
     * NACKs a task (negative acknowledgment with retry).
     * 
     * @param taskId Task ID
     * @param delaySeconds Delay before retry
     * @param reason Reason for NACK
     * @throws CodeQException if NACK fails
     */
    public void nack(String taskId, int delaySeconds, String reason) throws CodeQException {
        try {
            Map<String, Object> body = Map.of(
                "delaySeconds", delaySeconds,
                "reason", reason
            );
            String json = objectMapper.writeValueAsString(body);
            
            Request request = new Request.Builder()
                .url(baseUrl + "/v1/codeq/tasks/" + taskId + "/nack")
                .header("Authorization", "Bearer " + workerToken)
                .header("Content-Type", "application/json")
                .post(RequestBody.create(json, JSON))
                .build();

            try (Response response = httpClient.newCall(request).execute()) {
                if (!response.isSuccessful()) {
                    throw new CodeQException("Failed to NACK task: " + response.code() + " " + response.message());
                }
            }
        } catch (IOException e) {
            throw new CodeQException("Error NACKing task", e);
        }
    }

    private static OkHttpClient createDefaultHttpClient() {
        return new OkHttpClient.Builder()
            .connectTimeout(Duration.ofSeconds(10))
            .readTimeout(Duration.ofSeconds(30))
            .writeTimeout(Duration.ofSeconds(30))
            .build();
    }

    public static Builder builder() {
        return new Builder();
    }

    /**
     * Builder for CodeQClient
     */
    public static class Builder {
        private String baseUrl;
        private String producerToken;
        private String workerToken;
        private OkHttpClient httpClient;

        public Builder baseUrl(String baseUrl) {
            this.baseUrl = baseUrl;
            return this;
        }

        public Builder producerToken(String token) {
            this.producerToken = token;
            return this;
        }

        public Builder workerToken(String token) {
            this.workerToken = token;
            return this;
        }

        public Builder httpClient(OkHttpClient client) {
            this.httpClient = client;
            return this;
        }

        public CodeQClient build() {
            if (baseUrl == null) {
                throw new IllegalArgumentException("baseUrl is required");
            }
            return new CodeQClient(this);
        }
    }
}
