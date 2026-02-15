package io.codeq.sdk;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.time.Instant;
import java.util.Map;

/**
 * Represents a task in the CodeQ system.
 * Tasks are units of work that can be enqueued, claimed, and processed by workers.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class Task {
    
    @JsonProperty("id")
    private String id;
    
    @JsonProperty("command")
    private String command;
    
    @JsonProperty("payload")
    private Map<String, Object> payload;
    
    @JsonProperty("priority")
    private Integer priority;
    
    @JsonProperty("webhook")
    private String webhook;
    
    @JsonProperty("status")
    private TaskStatus status;
    
    @JsonProperty("workerId")
    private String workerId;
    
    @JsonProperty("leaseUntil")
    private Instant leaseUntil;
    
    @JsonProperty("attempts")
    private Integer attempts;
    
    @JsonProperty("maxAttempts")
    private Integer maxAttempts;
    
    @JsonProperty("error")
    private String error;
    
    @JsonProperty("resultKey")
    private String resultKey;
    
    @JsonProperty("createdAt")
    private Instant createdAt;
    
    @JsonProperty("updatedAt")
    private Instant updatedAt;

    // Constructors
    public Task() {}

    public Task(String command, Map<String, Object> payload) {
        this.command = command;
        this.payload = payload;
    }

    // Getters and Setters
    public String getId() { return id; }
    public void setId(String id) { this.id = id; }

    public String getCommand() { return command; }
    public void setCommand(String command) { this.command = command; }

    public Map<String, Object> getPayload() { return payload; }
    public void setPayload(Map<String, Object> payload) { this.payload = payload; }

    public Integer getPriority() { return priority; }
    public void setPriority(Integer priority) { this.priority = priority; }

    public String getWebhook() { return webhook; }
    public void setWebhook(String webhook) { this.webhook = webhook; }

    public TaskStatus getStatus() { return status; }
    public void setStatus(TaskStatus status) { this.status = status; }

    public String getWorkerId() { return workerId; }
    public void setWorkerId(String workerId) { this.workerId = workerId; }

    public Instant getLeaseUntil() { return leaseUntil; }
    public void setLeaseUntil(Instant leaseUntil) { this.leaseUntil = leaseUntil; }

    public Integer getAttempts() { return attempts; }
    public void setAttempts(Integer attempts) { this.attempts = attempts; }

    public Integer getMaxAttempts() { return maxAttempts; }
    public void setMaxAttempts(Integer maxAttempts) { this.maxAttempts = maxAttempts; }

    public String getError() { return error; }
    public void setError(String error) { this.error = error; }

    public String getResultKey() { return resultKey; }
    public void setResultKey(String resultKey) { this.resultKey = resultKey; }

    public Instant getCreatedAt() { return createdAt; }
    public void setCreatedAt(Instant createdAt) { this.createdAt = createdAt; }

    public Instant getUpdatedAt() { return updatedAt; }
    public void setUpdatedAt(Instant updatedAt) { this.updatedAt = updatedAt; }

    /**
     * Task status enumeration
     */
    public enum TaskStatus {
        PENDING,
        IN_PROGRESS,
        COMPLETED,
        FAILED
    }
}
