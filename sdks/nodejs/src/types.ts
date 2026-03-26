/**
 * Task status enumeration
 */
export enum TaskStatus {
  PENDING = 'PENDING',
  IN_PROGRESS = 'IN_PROGRESS',
  COMPLETED = 'COMPLETED',
  FAILED = 'FAILED',
}

/**
 * Represents a task in the CodeQ system.
 */
export interface Task {
  /** Unique task identifier (UUID) */
  id: string;
  /** Command name that identifies the task type */
  command: string;
  /** Task payload as a JSON-serialized string or parsed object */
  payload: Record<string, any>;
  /** Task priority (higher values are processed first) */
  priority?: number;
  /** Optional webhook URL called on task completion */
  webhook?: string;
  /** Current task status */
  status: TaskStatus;
  /** ID of the worker that claimed this task */
  workerId?: string;
  /** RFC3339 timestamp when the lease expires */
  leaseUntil?: string;
  /** Number of processing attempts */
  attempts?: number;
  /** Maximum number of retry attempts */
  maxAttempts?: number;
  /** Error message if the task failed */
  error?: string;
  /** Key referencing stored result data */
  resultKey?: string;
  /** Tenant identifier for multi-tenant isolation */
  tenantId?: string;
  /** RFC3339 timestamp when the task was created */
  createdAt: string;
  /** RFC3339 timestamp when the task was last updated */
  updatedAt: string;
}

/**
 * Options for creating a task.
 */
export interface CreateTaskOptions {
  /** Command name that identifies the task type */
  command: string;
  /** Task payload (arbitrary JSON object) */
  payload: Record<string, any>;
  /** Task priority (higher values are processed first, default: 0) */
  priority?: number;
  /** Webhook URL to call on task completion */
  webhook?: string;
  /** Maximum number of retry attempts (default: 5) */
  maxAttempts?: number;
  /** Delay in seconds before the task becomes available */
  delaySeconds?: number;
  /** Idempotency key for deduplication (24h window) */
  idempotencyKey?: string;
  /** RFC3339 timestamp for scheduled execution */
  runAt?: string;
}

/**
 * Options for claiming a task.
 */
export interface ClaimTaskOptions {
  /** Command names to filter tasks by */
  commands: string[];
  /** Lease duration in seconds (default: 300) */
  leaseSeconds?: number;
  /** Long-poll wait time in seconds (max: 30, default: 0) */
  waitSeconds?: number;
}

/**
 * Artifact input for result submission.
 */
export interface ArtifactIn {
  /** Artifact filename */
  name: string;
  /** URL to externally hosted artifact */
  url?: string;
  /** Base64-encoded inline content */
  contentBase64?: string;
  /** MIME content type */
  contentType?: string;
}

/**
 * Artifact output from a task result.
 */
export interface ArtifactOut {
  /** Artifact filename */
  name: string;
  /** URL to the stored artifact */
  url: string;
}

/**
 * Options for submitting a task result.
 */
export interface SubmitResultOptions {
  /** Result status */
  status: 'COMPLETED' | 'FAILED';
  /** Result data (required if status is COMPLETED) */
  result?: Record<string, any>;
  /** Error message (required if status is FAILED) */
  error?: string;
  /** Optional artifacts to attach */
  artifacts?: ArtifactIn[];
}

/**
 * Response returned after submitting a result.
 */
export interface ResultRecord {
  /** ID of the associated task */
  taskId: string;
  /** Result status */
  status: TaskStatus;
  /** Result data */
  result?: Record<string, any>;
  /** Error message if failed */
  error?: string;
  /** Output artifacts */
  artifacts?: ArtifactOut[];
  /** RFC3339 timestamp when the result was recorded */
  completedAt: string;
}

/**
 * Response from getResult containing both task and result.
 */
export interface TaskResult {
  /** Full task object */
  task: Task;
  /** Result record */
  result: ResultRecord;
}

/**
 * Response from a NACK operation.
 */
export interface NackResponse {
  /** New task status: "requeued" or "dlq" */
  status: string;
  /** Delay in seconds before retry */
  delaySeconds: number;
}

/**
 * Queue statistics for a command.
 */
export interface QueueStats {
  /** Command name */
  command: string;
  /** Number of tasks ready for processing */
  ready: number;
  /** Number of delayed tasks */
  delayed: number;
  /** Number of tasks currently being processed */
  inProgress: number;
  /** Number of tasks in the dead letter queue */
  dlq: number;
}

/**
 * Options for creating a webhook subscription.
 */
export interface CreateSubscriptionOptions {
  /** Webhook callback URL */
  callbackUrl: string;
  /** Command types to subscribe to */
  eventTypes?: string[];
  /** Time-to-live in seconds (default: 300) */
  ttlSeconds?: number;
  /** Delivery mode: "fanout", "group", or "hash" */
  deliveryMode?: 'fanout' | 'group' | 'hash';
  /** Group identifier (required if deliveryMode is "group") */
  groupId?: string;
  /** Minimum interval between deliveries in seconds */
  minIntervalSeconds?: number;
}

/**
 * Response from subscription creation or renewal.
 */
export interface SubscriptionResponse {
  /** Unique subscription identifier */
  subscriptionId: string;
  /** RFC3339 timestamp when the subscription expires */
  expiresAt: string;
}

/**
 * Options for renewing a subscription.
 */
export interface RenewSubscriptionOptions {
  /** New TTL in seconds */
  ttlSeconds?: number;
}

/**
 * Options for the waitForResult polling method.
 */
export interface WaitForResultOptions {
  /** Maximum time to wait in milliseconds (default: 30000) */
  timeout?: number;
  /** Polling interval in milliseconds (default: 1000) */
  pollInterval?: number;
}

/**
 * Options for admin task cleanup.
 */
export interface CleanupOptions {
  /** Maximum number of tasks to clean up (default: 1000) */
  limit?: number;
  /** RFC3339 timestamp cutoff for cleanup (default: now) */
  before?: string;
}

/**
 * Response from admin task cleanup.
 */
export interface CleanupResult {
  /** Number of tasks deleted */
  deleted: number;
  /** Cutoff timestamp used */
  before: string;
  /** Limit that was applied */
  limit: number;
}
