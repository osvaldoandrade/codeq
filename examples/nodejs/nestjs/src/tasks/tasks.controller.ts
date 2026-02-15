import { Controller, Post, Body, HttpCode, HttpStatus } from '@nestjs/common';
import { TasksService } from './tasks.service';
import { Task } from '@codeq/sdk';

/**
 * Controller for task creation endpoints.
 * 
 * Exposes REST API for external systems to enqueue tasks.
 */
@Controller('tasks')
export class TasksController {
  constructor(private readonly tasksService: TasksService) {}

  /**
   * Creates a master generation task.
   * 
   * POST /api/tasks/master
   * Body: { "jobId": "123", "priority": 5 }
   */
  @Post('master')
  @HttpCode(HttpStatus.OK)
  async createMasterTask(
    @Body() body: { jobId: string; priority?: number },
  ): Promise<Task> {
    return this.tasksService.createMasterTask(body.jobId, body.priority);
  }

  /**
   * Creates a task with webhook.
   * 
   * POST /api/tasks/with-webhook
   * Body: { "command": "GENERATE_MASTER", "payload": {...}, "webhook": "https://..." }
   */
  @Post('with-webhook')
  @HttpCode(HttpStatus.OK)
  async createTaskWithWebhook(
    @Body()
    body: {
      command: string;
      payload: Record<string, any>;
      webhook: string;
    },
  ): Promise<Task> {
    return this.tasksService.createTaskWithWebhook(
      body.command,
      body.payload,
      body.webhook,
    );
  }

  /**
   * Creates a delayed task.
   * 
   * POST /api/tasks/delayed
   * Body: { "command": "GENERATE_MASTER", "payload": {...}, "delaySeconds": 60 }
   */
  @Post('delayed')
  @HttpCode(HttpStatus.OK)
  async createDelayedTask(
    @Body()
    body: {
      command: string;
      payload: Record<string, any>;
      delaySeconds: number;
    },
  ): Promise<Task> {
    return this.tasksService.createDelayedTask(
      body.command,
      body.payload,
      body.delaySeconds,
    );
  }
}
