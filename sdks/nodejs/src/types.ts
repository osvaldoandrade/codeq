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
 * Represents a task in the CodeQ system
 */
export interface Task {
  id: string;
  command: string;
  payload: Record<string, any>;
  priority?: number;
  webhook?: string;
  status: TaskStatus;
  workerId?: string;
  leaseUntil?: string;
  attempts?: number;
  maxAttempts?: number;
  error?: string;
  resultKey?: string;
  createdAt: string;
  updatedAt: string;
}

/**
 * Options for creating a task
 */
export interface CreateTaskOptions {
  command: string;
  payload: Record<string, any>;
  priority?: number;
  webhook?: string;
  maxAttempts?: number;
  delaySeconds?: number;
  idempotencyKey?: string;
}

/**
 * Options for claiming a task
 */
export interface ClaimTaskOptions {
  commands: string[];
  leaseSeconds?: number;
  waitSeconds?: number;
}

/**
 * Options for submitting a result
 */
export interface SubmitResultOptions {
  status: 'COMPLETED' | 'FAILED';
  result: Record<string, any>;
  error?: string;
}

/**
 * Queue statistics
 */
export interface QueueStats {
  command: string;
  ready: number;
  delayed: number;
  inProgress: number;
  dlq: number;
}
