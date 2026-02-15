import { Injectable, Inject, Logger } from '@nestjs/common';
import { CodeQClient, Task, CreateTaskOptions } from '@codeq/sdk';

/**
 * Service for producing tasks to CodeQ.
 * 
 * Provides methods for creating different types of tasks
 * with various configurations.
 */
@Injectable()
export class TasksService {
  private readonly logger = new Logger(TasksService.name);

  constructor(
    @Inject('CODEQ_CLIENT')
    private readonly codeQClient: CodeQClient,
  ) {}

  /**
   * Creates a master generation task.
   * 
   * @param jobId Job identifier
   * @param priority Task priority (0-9)
   * @returns Created task
   */
  async createMasterTask(jobId: string, priority: number = 5): Promise<Task> {
    this.logger.log(`Creating GENERATE_MASTER task for jobId: ${jobId}`);

    const options: CreateTaskOptions = {
      command: 'GENERATE_MASTER',
      payload: {
        jobId,
        timestamp: Date.now(),
      },
      priority,
    };

    const task = await this.codeQClient.createTask(options);
    this.logger.log(`Task created: ${task.id}`);
    
    return task;
  }

  /**
   * Creates a task with webhook callback.
   * 
   * @param command Command type
   * @param payload Task payload
   * @param webhookUrl Webhook URL
   * @returns Created task
   */
  async createTaskWithWebhook(
    command: string,
    payload: Record<string, any>,
    webhookUrl: string,
  ): Promise<Task> {
    this.logger.log(`Creating ${command} task with webhook: ${webhookUrl}`);

    const options: CreateTaskOptions = {
      command,
      payload,
      priority: 5,
      webhook: webhookUrl,
      maxAttempts: 3,
    };

    const task = await this.codeQClient.createTask(options);
    this.logger.log(`Task created with webhook: ${task.id}`);
    
    return task;
  }

  /**
   * Creates a delayed task.
   * 
   * @param command Command type
   * @param payload Task payload
   * @param delaySeconds Delay in seconds
   * @returns Created task
   */
  async createDelayedTask(
    command: string,
    payload: Record<string, any>,
    delaySeconds: number,
  ): Promise<Task> {
    this.logger.log(`Creating delayed ${command} task (delay: ${delaySeconds}s)`);

    const options: CreateTaskOptions = {
      command,
      payload,
      priority: 5,
      delaySeconds,
    };

    const task = await this.codeQClient.createTask(options);
    this.logger.log(`Delayed task created: ${task.id}`);
    
    return task;
  }
}
