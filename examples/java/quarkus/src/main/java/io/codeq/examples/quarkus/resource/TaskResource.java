package io.codeq.examples.quarkus.resource;

import io.codeq.examples.quarkus.service.TaskProducerService;
import io.codeq.sdk.CodeQException;
import io.codeq.sdk.Task;
import jakarta.inject.Inject;
import jakarta.ws.rs.*;
import jakarta.ws.rs.core.MediaType;
import jakarta.ws.rs.core.Response;

import java.util.Map;

/**
 * REST resource for task creation.
 * Exposes endpoints for external systems to enqueue tasks.
 */
@Path("/api/tasks")
@Produces(MediaType.APPLICATION_JSON)
@Consumes(MediaType.APPLICATION_JSON)
public class TaskResource {

    @Inject
    TaskProducerService producerService;

    /**
     * Creates a master generation task.
     * 
     * POST /api/tasks/master
     * Body: { "jobId": "123", "priority": 5 }
     */
    @POST
    @Path("/master")
    public Response createMasterTask(Map<String, Object> request) {
        try {
            String jobId = (String) request.get("jobId");
            int priority = (int) request.getOrDefault("priority", 5);

            Task task = producerService.createMasterTask(jobId, priority);
            return Response.ok(task).build();

        } catch (CodeQException e) {
            return Response.serverError()
                    .entity(Map.of("error", e.getMessage()))
                    .build();
        }
    }

    /**
     * Creates a task with webhook.
     * 
     * POST /api/tasks/with-webhook
     * Body: { "command": "GENERATE_MASTER", "payload": {...}, "webhook": "https://..." }
     */
    @POST
    @Path("/with-webhook")
    public Response createTaskWithWebhook(Map<String, Object> request) {
        try {
            String command = (String) request.get("command");
            @SuppressWarnings("unchecked")
            Map<String, Object> payload = (Map<String, Object>) request.get("payload");
            String webhook = (String) request.get("webhook");

            Task task = producerService.createTaskWithWebhook(command, payload, webhook);
            return Response.ok(task).build();

        } catch (CodeQException e) {
            return Response.serverError()
                    .entity(Map.of("error", e.getMessage()))
                    .build();
        }
    }

    /**
     * Health check endpoint.
     */
    @GET
    @Path("/health")
    public Response health() {
        return Response.ok(Map.of("status", "ok")).build();
    }
}
