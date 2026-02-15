import axios, { AxiosInstance, AxiosError } from 'axios';
import axiosRetry from 'axios-retry';
import {
  Task,
  CreateTaskOptions,
  ClaimTaskOptions,
  SubmitResultOptions,
  QueueStats,
} from './types';

/**
 * Configuration options for CodeQClient
 */
export interface CodeQClientConfig {
  baseUrl: string;
  producerToken?: string;
  workerToken?: string;
  timeout?: number;
  retries?: number;
}

/**
 * CodeQ Client for producing and consuming tasks.
 * 
 * This client provides methods to:
 * - Create tasks (producer role)
 * - Claim tasks (worker role)
 * - Submit results
 * - Manage task lifecycle (heartbeat, abandon, nack)
 * 
 * @example
 * ```typescript
 * const client = new CodeQClient({
 *   baseUrl: 'https://codeq.example.com',
 *   producerToken: 'your-producer-token',
 *   workerToken: 'your-worker-token',
 * });
 * 
 * // Create a task
 * const task = await client.createTask({
 *   command: 'GENERATE_MASTER',
 *   payload: { jobId: '123' },
 *   priority: 5,
 * });
 * 
 * // Claim a task
 * const claimed = await client.claimTask({
 *   commands: ['GENERATE_MASTER'],
 *   leaseSeconds: 120,
 *   waitSeconds: 10,
 * });
 * 
 * // Submit result
 * if (claimed) {
 *   await client.submitResult(claimed.id, {
 *     status: 'COMPLETED',
 *     result: { success: true },
 *   });
 * }
 * ```
 */
export class CodeQClient {
  private readonly httpClient: AxiosInstance;
  private readonly producerToken?: string;
  private readonly workerToken?: string;

  constructor(config: CodeQClientConfig) {
    const baseURL = config.baseUrl.endsWith('/')
      ? config.baseUrl.slice(0, -1)
      : config.baseUrl;

    this.producerToken = config.producerToken;
    this.workerToken = config.workerToken;

    this.httpClient = axios.create({
      baseURL,
      timeout: config.timeout || 30000,
      headers: {
        'Content-Type': 'application/json',
      },
    });

    // Configure retry logic
    axiosRetry(this.httpClient, {
      retries: config.retries || 3,
      retryDelay: axiosRetry.exponentialDelay,
      retryCondition: (error: AxiosError) => {
        return (
          axiosRetry.isNetworkOrIdempotentRequestError(error) ||
          (error.response?.status ? error.response.status >= 500 : false)
        );
      },
    });
  }

  /**
   * Creates a new task in the queue.
   * 
   * @param options Task creation options
   * @returns Created task with ID
   * @throws Error if task creation fails
   */
  async createTask(options: CreateTaskOptions): Promise<Task> {
    if (!this.producerToken) {
      throw new Error('Producer token is required to create tasks');
    }

    try {
      const response = await this.httpClient.post<Task>(
        '/v1/codeq/tasks',
        options,
        {
          headers: {
            Authorization: `Bearer ${this.producerToken}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to create task', error);
    }
  }

  /**
   * Claims a task from the queue (worker operation).
   * 
   * @param options Claim options
   * @returns Claimed task or null if no task available
   * @throws Error if claim fails
   */
  async claimTask(options: ClaimTaskOptions): Promise<Task | null> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to claim tasks');
    }

    try {
      const response = await this.httpClient.post<Task>(
        '/v1/codeq/tasks/claim',
        {
          commands: options.commands,
          leaseSeconds: options.leaseSeconds || 120,
          waitSeconds: options.waitSeconds || 0,
        },
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      if (axios.isAxiosError(error) && error.response?.status === 204) {
        return null; // No task available
      }
      throw this.handleError('Failed to claim task', error);
    }
  }

  /**
   * Submits a result for a completed task.
   * 
   * @param taskId Task ID
   * @param options Result submission options
   * @throws Error if submission fails
   */
  async submitResult(
    taskId: string,
    options: SubmitResultOptions
  ): Promise<void> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to submit results');
    }

    try {
      await this.httpClient.post(
        `/v1/codeq/tasks/${taskId}/result`,
        options,
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
    } catch (error) {
      throw this.handleError('Failed to submit result', error);
    }
  }

  /**
   * Extends the lease on a task (heartbeat).
   * 
   * @param taskId Task ID
   * @param extendSeconds Seconds to extend lease
   * @throws Error if heartbeat fails
   */
  async heartbeat(taskId: string, extendSeconds: number = 60): Promise<void> {
    if (!this.workerToken) {
      throw new Error('Worker token is required for heartbeat');
    }

    try {
      await this.httpClient.post(
        `/v1/codeq/tasks/${taskId}/heartbeat`,
        { extendSeconds },
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
    } catch (error) {
      throw this.handleError('Failed to send heartbeat', error);
    }
  }

  /**
   * Abandons a task (returns it to the queue).
   * 
   * @param taskId Task ID
   * @throws Error if abandon fails
   */
  async abandon(taskId: string): Promise<void> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to abandon tasks');
    }

    try {
      await this.httpClient.post(
        `/v1/codeq/tasks/${taskId}/abandon`,
        {},
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
    } catch (error) {
      throw this.handleError('Failed to abandon task', error);
    }
  }

  /**
   * NACKs a task (negative acknowledgment with retry).
   * 
   * @param taskId Task ID
   * @param delaySeconds Delay before retry
   * @param reason Reason for NACK
   * @throws Error if NACK fails
   */
  async nack(
    taskId: string,
    delaySeconds: number,
    reason: string
  ): Promise<void> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to NACK tasks');
    }

    try {
      await this.httpClient.post(
        `/v1/codeq/tasks/${taskId}/nack`,
        { delaySeconds, reason },
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
    } catch (error) {
      throw this.handleError('Failed to NACK task', error);
    }
  }

  /**
   * Gets task by ID.
   * 
   * @param taskId Task ID
   * @returns Task details
   * @throws Error if retrieval fails
   */
  async getTask(taskId: string): Promise<Task> {
    const token = this.workerToken || this.producerToken;
    if (!token) {
      throw new Error('Token is required to get task');
    }

    try {
      const response = await this.httpClient.get<Task>(
        `/v1/codeq/tasks/${taskId}`,
        {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to get task', error);
    }
  }

  /**
   * Gets queue statistics for a command.
   * 
   * @param command Command name
   * @returns Queue statistics
   * @throws Error if retrieval fails
   */
  async getQueueStats(command: string): Promise<QueueStats> {
    const token = this.workerToken || this.producerToken;
    if (!token) {
      throw new Error('Token is required to get queue stats');
    }

    try {
      const response = await this.httpClient.get<QueueStats>(
        `/v1/codeq/admin/queues/${command}/stats`,
        {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to get queue stats', error);
    }
  }

  private handleError(message: string, error: unknown): Error {
    if (axios.isAxiosError(error)) {
      const status = error.response?.status;
      const data = error.response?.data;
      return new Error(
        `${message}: ${status} - ${JSON.stringify(data) || error.message}`
      );
    }
    return new Error(`${message}: ${error}`);
  }
}
