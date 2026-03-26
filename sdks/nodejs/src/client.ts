import axios, { AxiosInstance, AxiosError } from 'axios';
import axiosRetry from 'axios-retry';
import {
  Task,
  TaskResult,
  ResultRecord,
  CreateTaskOptions,
  ClaimTaskOptions,
  SubmitResultOptions,
  NackResponse,
  QueueStats,
  CreateSubscriptionOptions,
  SubscriptionResponse,
  RenewSubscriptionOptions,
  WaitForResultOptions,
  CleanupOptions,
  CleanupResult,
} from './types';

/**
 * Configuration options for the CodeQ client.
 */
export interface CodeQClientConfig {
  /** Base URL of the CodeQ server (e.g., "http://localhost:8080") */
  baseUrl: string;
  /** JWT token for producer operations (createTask) */
  producerToken?: string;
  /** JWT token for worker operations (claim, heartbeat, result, subscribe) */
  workerToken?: string;
  /** JWT token for admin operations (queue stats, cleanup) */
  adminToken?: string;
  /** Request timeout in milliseconds (default: 30000) */
  timeout?: number;
  /** Number of automatic retries on transient failures (default: 3) */
  retries?: number;
}

/**
 * Official CodeQ client for producing and consuming tasks.
 *
 * Provides a complete, Promise-based API for interacting with the CodeQ
 * task scheduling server. Supports producer, worker, and admin operations
 * with automatic retry logic and exponential backoff.
 *
 * @example
 * ```typescript
 * import { CodeQClient } from '@osvaldoandrade/codeq-client';
 *
 * const client = new CodeQClient({
 *   baseUrl: 'http://localhost:8080',
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
 * // Wait for result
 * const result = await client.waitForResult(task.id, { timeout: 60000 });
 * console.log(result.status, result.result);
 * ```
 */
export class CodeQClient {
  private readonly httpClient: AxiosInstance;
  private readonly producerToken?: string;
  private readonly workerToken?: string;
  private readonly adminToken?: string;

  constructor(config: CodeQClientConfig) {
    const baseURL = config.baseUrl.endsWith('/')
      ? config.baseUrl.slice(0, -1)
      : config.baseUrl;

    this.producerToken = config.producerToken;
    this.workerToken = config.workerToken;
    this.adminToken = config.adminToken;

    this.httpClient = axios.create({
      baseURL,
      timeout: config.timeout || 30000,
      headers: {
        'Content-Type': 'application/json',
      },
    });

    // Configure retry logic with exponential backoff
    axiosRetry(this.httpClient, {
      retries: config.retries ?? 3,
      retryDelay: axiosRetry.exponentialDelay,
      retryCondition: (error: AxiosError) => {
        return (
          axiosRetry.isNetworkOrIdempotentRequestError(error) ||
          (error.response?.status ? error.response.status >= 500 : false)
        );
      },
    });
  }

  // ──────────────────────────────────────────────
  // Producer Operations
  // ──────────────────────────────────────────────

  /**
   * Creates a new task in the queue.
   *
   * Requires a producer token. The task is placed into the appropriate queue
   * based on its command and becomes available for workers to claim.
   *
   * @param options - Task creation options including command, payload, and priority
   * @returns The created task with its assigned ID and initial status
   * @throws Error if producer token is missing or the request fails
   *
   * @example
   * ```typescript
   * const task = await client.createTask({
   *   command: 'GENERATE_MASTER',
   *   payload: { jobId: 'j-123' },
   *   priority: 5,
   *   maxAttempts: 3,
   * });
   * console.log('Created task:', task.id);
   * ```
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

  // ──────────────────────────────────────────────
  // Worker Operations
  // ──────────────────────────────────────────────

  /**
   * Claims a task from the queue for processing.
   *
   * Requires a worker token with `codeq:claim` scope. Supports long-polling
   * via `waitSeconds` to avoid busy-waiting when no tasks are available.
   *
   * @param options - Claim options including command filter and lease duration
   * @returns The claimed task, or `null` if no task is available (HTTP 204)
   * @throws Error if worker token is missing or the request fails
   *
   * @example
   * ```typescript
   * const task = await client.claimTask({
   *   commands: ['GENERATE_MASTER'],
   *   leaseSeconds: 120,
   *   waitSeconds: 10,
   * });
   * if (task) {
   *   console.log('Claimed:', task.id);
   * }
   * ```
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
          leaseSeconds: options.leaseSeconds ?? 300,
          waitSeconds: options.waitSeconds ?? 0,
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
        return null;
      }
      throw this.handleError('Failed to claim task', error);
    }
  }

  /**
   * Submits a result for a completed or failed task.
   *
   * Requires a worker token with `codeq:result` scope.
   *
   * @param taskId - ID of the task to submit a result for
   * @param options - Result submission options including status and data
   * @returns The result record
   * @throws Error if worker token is missing or the request fails
   *
   * @example
   * ```typescript
   * await client.submitResult(task.id, {
   *   status: 'COMPLETED',
   *   result: { output: 'done' },
   * });
   * ```
   */
  async submitResult(
    taskId: string,
    options: SubmitResultOptions
  ): Promise<ResultRecord> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to submit results');
    }

    try {
      const response = await this.httpClient.post<ResultRecord>(
        `/v1/codeq/tasks/${encodeURIComponent(taskId)}/result`,
        options,
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to submit result', error);
    }
  }

  /**
   * Sends a heartbeat to extend the lease on a claimed task.
   *
   * Requires a worker token with `codeq:heartbeat` scope. Workers should
   * send heartbeats periodically to prevent lease expiration.
   *
   * @param taskId - ID of the task to extend the lease for
   * @param extendSeconds - Number of seconds to extend the lease (default: 300)
   * @throws Error if worker token is missing or the request fails
   *
   * @example
   * ```typescript
   * await client.heartbeat(task.id, 120);
   * ```
   */
  async heartbeat(taskId: string, extendSeconds: number = 300): Promise<void> {
    if (!this.workerToken) {
      throw new Error('Worker token is required for heartbeat');
    }

    try {
      await this.httpClient.post(
        `/v1/codeq/tasks/${encodeURIComponent(taskId)}/heartbeat`,
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
   * Abandons a claimed task, returning it to the queue for another worker.
   *
   * Requires a worker token with `codeq:abandon` scope.
   *
   * @param taskId - ID of the task to abandon
   * @throws Error if worker token is missing or the request fails
   *
   * @example
   * ```typescript
   * await client.abandon(task.id);
   * ```
   */
  async abandon(taskId: string): Promise<void> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to abandon tasks');
    }

    try {
      await this.httpClient.post(
        `/v1/codeq/tasks/${encodeURIComponent(taskId)}/abandon`,
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
   * Sends a negative acknowledgment (NACK) for a task with optional backoff.
   *
   * The task is re-queued with a delay. If the task has exceeded its
   * `maxAttempts`, it is moved to the dead letter queue (DLQ).
   *
   * Requires a worker token with `codeq:nack` scope.
   *
   * @param taskId - ID of the task to NACK
   * @param delaySeconds - Delay in seconds before the task is retried
   * @param reason - Reason for the negative acknowledgment
   * @returns NACK response indicating whether the task was requeued or moved to DLQ
   * @throws Error if worker token is missing or the request fails
   *
   * @example
   * ```typescript
   * const resp = await client.nack(task.id, 30, 'temporary failure');
   * console.log(resp.status); // "requeued" or "dlq"
   * ```
   */
  async nack(
    taskId: string,
    delaySeconds: number,
    reason: string
  ): Promise<NackResponse> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to NACK tasks');
    }

    try {
      const response = await this.httpClient.post<NackResponse>(
        `/v1/codeq/tasks/${encodeURIComponent(taskId)}/nack`,
        { delaySeconds, reason },
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to NACK task', error);
    }
  }

  // ──────────────────────────────────────────────
  // Subscription (Webhook) Operations
  // ──────────────────────────────────────────────

  /**
   * Registers a webhook subscription for push-based task delivery.
   *
   * Requires a worker token with `codeq:subscribe` scope. The server will
   * POST task notifications to the specified callback URL.
   *
   * @param options - Subscription options including callback URL and event types
   * @returns Subscription response with ID and expiration time
   * @throws Error if worker token is missing or the request fails
   *
   * @example
   * ```typescript
   * const sub = await client.createSubscription({
   *   callbackUrl: 'https://myapp.com/webhook',
   *   eventTypes: ['GENERATE_MASTER'],
   *   ttlSeconds: 3600,
   *   deliveryMode: 'group',
   *   groupId: 'worker-pool-1',
   * });
   * console.log('Subscription:', sub.subscriptionId);
   * ```
   */
  async createSubscription(
    options: CreateSubscriptionOptions
  ): Promise<SubscriptionResponse> {
    if (!this.workerToken) {
      throw new Error('Worker token is required to create subscriptions');
    }

    try {
      const response = await this.httpClient.post<SubscriptionResponse>(
        '/v1/codeq/workers/subscriptions',
        options,
        {
          headers: {
            Authorization: `Bearer ${this.workerToken}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to create subscription', error);
    }
  }

  /**
   * Renews an existing webhook subscription to extend its TTL.
   *
   * @param subscriptionId - ID of the subscription to renew
   * @param options - Optional renewal options (e.g., new TTL)
   * @returns Updated subscription response with new expiration time
   * @throws Error if token is missing or the request fails
   *
   * @example
   * ```typescript
   * const renewed = await client.renewSubscription('sub-123', {
   *   ttlSeconds: 7200,
   * });
   * console.log('Expires at:', renewed.expiresAt);
   * ```
   */
  async renewSubscription(
    subscriptionId: string,
    options?: RenewSubscriptionOptions
  ): Promise<SubscriptionResponse> {
    const token = this.workerToken || this.producerToken;
    if (!token) {
      throw new Error('Token is required to renew subscriptions');
    }

    try {
      const response = await this.httpClient.post<SubscriptionResponse>(
        `/v1/codeq/workers/subscriptions/${encodeURIComponent(subscriptionId)}/heartbeat`,
        options || {},
        {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to renew subscription', error);
    }
  }

  // ──────────────────────────────────────────────
  // Query Operations
  // ──────────────────────────────────────────────

  /**
   * Retrieves a task by its ID.
   *
   * @param taskId - ID of the task to retrieve
   * @returns The task details
   * @throws Error if token is missing or the request fails
   *
   * @example
   * ```typescript
   * const task = await client.getTask('abc-123');
   * console.log(task.status);
   * ```
   */
  async getTask(taskId: string): Promise<Task> {
    const token = this.workerToken || this.producerToken;
    if (!token) {
      throw new Error('Token is required to get task');
    }

    try {
      const response = await this.httpClient.get<Task>(
        `/v1/codeq/tasks/${encodeURIComponent(taskId)}`,
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
   * Retrieves the result for a completed or failed task.
   *
   * @param taskId - ID of the task to get the result for
   * @returns Task result containing both the task and result record
   * @throws Error if token is missing or the request fails
   *
   * @example
   * ```typescript
   * const { task, result } = await client.getResult('abc-123');
   * console.log(result.status, result.result);
   * ```
   */
  async getResult(taskId: string): Promise<TaskResult> {
    const token = this.workerToken || this.producerToken;
    if (!token) {
      throw new Error('Token is required to get result');
    }

    try {
      const response = await this.httpClient.get<TaskResult>(
        `/v1/codeq/tasks/${encodeURIComponent(taskId)}/result`,
        {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to get result', error);
    }
  }

  /**
   * Polls for a task result until it is available or a timeout is reached.
   *
   * This is a convenience method that repeatedly calls {@link getResult} until
   * a result is available. Useful for producers that need to wait for
   * task completion.
   *
   * @param taskId - ID of the task to wait for
   * @param options - Polling options (timeout and interval)
   * @returns The task result
   * @throws Error if the timeout is reached or polling fails
   *
   * @example
   * ```typescript
   * const result = await client.waitForResult(task.id, {
   *   timeout: 60000,
   *   pollInterval: 2000,
   * });
   * console.log(result.result.status, result.result.result);
   * ```
   */
  async waitForResult(
    taskId: string,
    options?: WaitForResultOptions
  ): Promise<TaskResult> {
    const timeout = options?.timeout ?? 30000;
    const pollInterval = options?.pollInterval ?? 1000;
    const deadline = Date.now() + timeout;

    while (Date.now() < deadline) {
      try {
        return await this.getResult(taskId);
      } catch {
        // Result not yet available, continue polling
      }

      const remaining = deadline - Date.now();
      if (remaining <= 0) {
        break;
      }

      await this.sleep(Math.min(pollInterval, remaining));
    }

    throw new Error(
      `Timed out waiting for result of task ${taskId} after ${timeout}ms`
    );
  }

  // ──────────────────────────────────────────────
  // Admin Operations
  // ──────────────────────────────────────────────

  /**
   * Lists statistics for all queues.
   *
   * Requires an admin token with `admin` scope.
   *
   * @returns Array of queue statistics
   * @throws Error if admin token is missing or the request fails
   *
   * @example
   * ```typescript
   * const queues = await client.listQueues();
   * queues.forEach(q => console.log(q.command, q.ready));
   * ```
   */
  async listQueues(): Promise<QueueStats[]> {
    const token = this.adminToken;
    if (!token) {
      throw new Error('Admin token is required to list queues');
    }

    try {
      const response = await this.httpClient.get<QueueStats[]>(
        '/v1/codeq/admin/queues',
        {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to list queues', error);
    }
  }

  /**
   * Gets statistics for a specific queue by command name.
   *
   * Requires an admin token with `admin` scope.
   *
   * @param command - Command name to get statistics for
   * @returns Queue statistics for the specified command
   * @throws Error if admin token is missing or the request fails
   *
   * @example
   * ```typescript
   * const stats = await client.getQueueStats('GENERATE_MASTER');
   * console.log('Ready:', stats.ready, 'In Progress:', stats.inProgress);
   * ```
   */
  async getQueueStats(command: string): Promise<QueueStats> {
    const token = this.adminToken;
    if (!token) {
      throw new Error('Admin token is required to get queue stats');
    }

    try {
      const response = await this.httpClient.get<QueueStats>(
        `/v1/codeq/admin/queues/${encodeURIComponent(command)}`,
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

  /**
   * Cleans up expired tasks.
   *
   * Requires an admin token with `admin` scope.
   *
   * @param options - Cleanup options (limit and cutoff timestamp)
   * @returns Cleanup result with the number of deleted tasks
   * @throws Error if admin token is missing or the request fails
   *
   * @example
   * ```typescript
   * const result = await client.cleanupExpired({ limit: 500 });
   * console.log('Deleted:', result.deleted);
   * ```
   */
  async cleanupExpired(options?: CleanupOptions): Promise<CleanupResult> {
    const token = this.adminToken;
    if (!token) {
      throw new Error('Admin token is required for cleanup');
    }

    try {
      const response = await this.httpClient.post<CleanupResult>(
        '/v1/codeq/admin/tasks/cleanup',
        options || {},
        {
          headers: {
            Authorization: `Bearer ${token}`,
          },
        }
      );
      return response.data;
    } catch (error) {
      throw this.handleError('Failed to cleanup expired tasks', error);
    }
  }

  // ──────────────────────────────────────────────
  // Internal Helpers
  // ──────────────────────────────────────────────

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

  private sleep(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }
}
