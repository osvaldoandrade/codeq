import { Injectable, Inject, Logger } from '@nestjs/common';
import { Cron, CronExpression } from '@nestjs/schedule';
import { CodeQClient, Task } from '@codeq/sdk';

/**
 * Worker service that polls and processes tasks from CodeQ.
 * 
 * Features:
 * - Polls for tasks every 5 seconds
 * - Claims tasks with 120s lease
 * - Sends heartbeats every 30s
 * - Processes tasks and submits results
 * - Handles errors with NACK
 */
@Injectable()
export class WorkerService {
  private readonly logger = new Logger(WorkerService.name);
  private currentTask: Task | null = null;

  constructor(
    @Inject('CODEQ_CLIENT')
    private readonly codeQClient: CodeQClient,
  ) {}

  /**
   * Polls for tasks and processes them.
   * Runs every 5 seconds.
   */
  @Cron(CronExpression.EVERY_5_SECONDS)
  async pollAndProcess(): Promise<void> {
    if (this.currentTask) {
      this.logger.debug(`Already processing task: ${this.currentTask.id}`);
      return;
    }

    try {
      // Claim a task with long-polling
      const task = await this.codeQClient.claimTask({
        commands: ['GENERATE_MASTER', 'GENERATE_CREATIVE'],
        leaseSeconds: 120,
        waitSeconds: 10,
      });

      if (!task) {
        this.logger.debug('No tasks available');
        return;
      }

      this.currentTask = task;
      this.logger.log(`Claimed task: ${task.id} (command: ${task.command})`);

      // Process the task
      await this.processTask(task);
    } catch (error) {
      this.logger.error('Error claiming task', error);
    }
  }

  /**
   * Sends heartbeat for current task.
   * Runs every 30 seconds.
   */
  @Cron(CronExpression.EVERY_30_SECONDS)
  async sendHeartbeat(): Promise<void> {
    if (!this.currentTask) {
      return;
    }

    try {
      await this.codeQClient.heartbeat(this.currentTask.id, 60);
      this.logger.debug(`Heartbeat sent for task: ${this.currentTask.id}`);
    } catch (error) {
      this.logger.error(
        `Error sending heartbeat for task: ${this.currentTask.id}`,
        error,
      );
    }
  }

  /**
   * Processes a task based on its command type.
   */
  private async processTask(task: Task): Promise<void> {
    try {
      this.logger.log(`Processing task: ${task.id} with payload:`, task.payload);

      // Process based on command
      let result: Record<string, any>;
      
      switch (task.command) {
        case 'GENERATE_MASTER':
          result = await this.processMasterGeneration(task);
          break;
        case 'GENERATE_CREATIVE':
          result = await this.processCreativeGeneration(task);
          break;
        default:
          throw new Error(`Unknown command: ${task.command}`);
      }

      // Submit successful result
      await this.codeQClient.submitResult(task.id, {
        status: 'COMPLETED',
        result,
      });
      
      this.logger.log(`Task completed successfully: ${task.id}`);
    } catch (error) {
      this.logger.error(`Error processing task: ${task.id}`, error);
      await this.handleTaskError(task, error);
    } finally {
      this.currentTask = null;
    }
  }

  /**
   * Processes GENERATE_MASTER command.
   */
  private async processMasterGeneration(
    task: Task,
  ): Promise<Record<string, any>> {
    const jobId = task.payload.jobId;
    this.logger.log(`Generating master for jobId: ${jobId}`);

    // Simulate work
    await this.sleep(5000);

    return {
      status: 'success',
      jobId,
      masterUrl: `https://storage.example.com/masters/${jobId}.mp4`,
      duration: 120.5,
      processedAt: Date.now(),
    };
  }

  /**
   * Processes GENERATE_CREATIVE command.
   */
  private async processCreativeGeneration(
    task: Task,
  ): Promise<Record<string, any>> {
    this.logger.log('Generating creative content');

    // Simulate work
    await this.sleep(3000);

    return {
      status: 'success',
      creativeUrl: 'https://storage.example.com/creatives/creative-123.jpg',
      processedAt: Date.now(),
    };
  }

  /**
   * Handles task processing errors.
   */
  private async handleTaskError(task: Task, error: any): Promise<void> {
    try {
      // Check if error is retryable
      if (this.isRetryable(error)) {
        // NACK with exponential backoff
        const delaySeconds = this.calculateBackoff(task.attempts || 0);
        await this.codeQClient.nack(
          task.id,
          delaySeconds,
          error.message || 'Unknown error',
        );
        this.logger.warn(
          `Task NACKed for retry: ${task.id} (delay: ${delaySeconds}s)`,
        );
      } else {
        // Submit failed result
        await this.codeQClient.submitResult(task.id, {
          status: 'FAILED',
          result: { error: error.message },
          error: error.message,
        });
        this.logger.error(`Task failed permanently: ${task.id}`);
      }
    } catch (err) {
      this.logger.error(`Error handling task failure: ${task.id}`, err);
    }
  }

  /**
   * Determines if an error is retryable.
   */
  private isRetryable(error: any): boolean {
    // Retry on transient errors, not on business logic errors
    return error.name !== 'ValidationError';
  }

  /**
   * Calculates backoff delay based on attempt count.
   */
  private calculateBackoff(attempts: number): number {
    return Math.min(300, Math.pow(2, attempts) * 5); // Max 5 minutes
  }

  /**
   * Sleep utility.
   */
  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}
