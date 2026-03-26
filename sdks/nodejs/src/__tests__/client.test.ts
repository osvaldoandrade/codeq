import axios from 'axios';
import { CodeQClient, CodeQClientConfig } from '../client';
import { TaskStatus } from '../types';

// Mock axios and axios-retry
jest.mock('axios', () => {
  const mockAxiosInstance = {
    get: jest.fn(),
    post: jest.fn(),
    interceptors: {
      request: { use: jest.fn() },
      response: { use: jest.fn() },
    },
  };

  const mockAxios: any = {
    create: jest.fn(() => mockAxiosInstance),
    isAxiosError: jest.fn((err: any) => err?.isAxiosError === true),
  };

  return {
    __esModule: true,
    default: mockAxios,
  };
});

jest.mock('axios-retry', () => ({
  __esModule: true,
  default: jest.fn(),
  exponentialDelay: jest.fn(),
  isNetworkOrIdempotentRequestError: jest.fn(),
}));

function createClient(overrides?: Partial<CodeQClientConfig>): CodeQClient {
  return new CodeQClient({
    baseUrl: 'http://localhost:8080',
    producerToken: 'producer-token',
    workerToken: 'worker-token',
    adminToken: 'admin-token',
    ...overrides,
  });
}

function getMockInstance() {
  return (axios.create as jest.Mock).mock.results[
    (axios.create as jest.Mock).mock.results.length - 1
  ].value;
}

describe('CodeQClient', () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  // ──────────────────────────────────────────────
  // Constructor
  // ──────────────────────────────────────────────

  describe('constructor', () => {
    it('creates an axios instance with correct config', () => {
      createClient();
      expect(axios.create).toHaveBeenCalledWith(
        expect.objectContaining({
          baseURL: 'http://localhost:8080',
          timeout: 30000,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });

    it('strips trailing slash from baseUrl', () => {
      createClient({ baseUrl: 'http://localhost:8080/' });
      expect(axios.create).toHaveBeenCalledWith(
        expect.objectContaining({
          baseURL: 'http://localhost:8080',
        })
      );
    });

    it('applies custom timeout', () => {
      createClient({ timeout: 5000 });
      expect(axios.create).toHaveBeenCalledWith(
        expect.objectContaining({ timeout: 5000 })
      );
    });
  });

  // ──────────────────────────────────────────────
  // createTask
  // ──────────────────────────────────────────────

  describe('createTask', () => {
    it('sends POST to /v1/codeq/tasks with producer token', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const taskData = {
        id: 'task-1',
        command: 'CMD',
        payload: {},
        status: TaskStatus.PENDING,
        createdAt: '2026-01-01T00:00:00Z',
        updatedAt: '2026-01-01T00:00:00Z',
      };
      mock.post.mockResolvedValue({ data: taskData });

      const result = await client.createTask({
        command: 'CMD',
        payload: { key: 'val' },
        priority: 5,
      });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks',
        { command: 'CMD', payload: { key: 'val' }, priority: 5 },
        { headers: { Authorization: 'Bearer producer-token' } }
      );
      expect(result).toEqual(taskData);
    });

    it('passes runAt and idempotencyKey when provided', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({ data: { id: 'task-2' } });

      await client.createTask({
        command: 'CMD',
        payload: {},
        runAt: '2026-06-01T00:00:00Z',
        idempotencyKey: 'idem-1',
      });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks',
        expect.objectContaining({
          runAt: '2026-06-01T00:00:00Z',
          idempotencyKey: 'idem-1',
        }),
        expect.any(Object)
      );
    });

    it('throws if producer token is missing', async () => {
      const client = createClient({ producerToken: undefined });
      await expect(
        client.createTask({ command: 'CMD', payload: {} })
      ).rejects.toThrow('Producer token is required to create tasks');
    });

    it('wraps axios errors with status and data', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const axiosErr: any = {
        isAxiosError: true,
        response: { status: 400, data: { error: 'bad request' } },
        message: 'Request failed',
      };
      mock.post.mockRejectedValue(axiosErr);
      (axios.isAxiosError as unknown as jest.Mock).mockReturnValue(true);

      await expect(
        client.createTask({ command: 'CMD', payload: {} })
      ).rejects.toThrow('Failed to create task: 400');
    });

    it('wraps non-axios errors', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockRejectedValue(new TypeError('network error'));
      (axios.isAxiosError as unknown as jest.Mock).mockReturnValue(false);

      await expect(
        client.createTask({ command: 'CMD', payload: {} })
      ).rejects.toThrow('Failed to create task:');
    });
  });

  // ──────────────────────────────────────────────
  // claimTask
  // ──────────────────────────────────────────────

  describe('claimTask', () => {
    it('sends POST to /v1/codeq/tasks/claim with worker token', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const taskData = { id: 'task-1', status: TaskStatus.IN_PROGRESS };
      mock.post.mockResolvedValue({ data: taskData });

      const result = await client.claimTask({
        commands: ['CMD_A'],
        leaseSeconds: 120,
        waitSeconds: 10,
      });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/claim',
        { commands: ['CMD_A'], leaseSeconds: 120, waitSeconds: 10 },
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(taskData);
    });

    it('applies default leaseSeconds and waitSeconds', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({ data: { id: 'task-1' } });

      await client.claimTask({ commands: ['CMD'] });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/claim',
        { commands: ['CMD'], leaseSeconds: 300, waitSeconds: 0 },
        expect.any(Object)
      );
    });

    it('returns null on 204 response', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const axiosErr: any = {
        isAxiosError: true,
        response: { status: 204 },
      };
      mock.post.mockRejectedValue(axiosErr);
      (axios.isAxiosError as unknown as jest.Mock).mockReturnValue(true);

      const result = await client.claimTask({ commands: ['CMD'] });
      expect(result).toBeNull();
    });

    it('throws if worker token is missing', async () => {
      const client = createClient({ workerToken: undefined });
      await expect(
        client.claimTask({ commands: ['CMD'] })
      ).rejects.toThrow('Worker token is required to claim tasks');
    });
  });

  // ──────────────────────────────────────────────
  // submitResult
  // ──────────────────────────────────────────────

  describe('submitResult', () => {
    it('sends POST with result data and returns ResultRecord', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const resultData = {
        taskId: 'task-1',
        status: TaskStatus.COMPLETED,
        result: { ok: true },
        completedAt: '2026-01-01T00:00:00Z',
      };
      mock.post.mockResolvedValue({ data: resultData });

      const result = await client.submitResult('task-1', {
        status: 'COMPLETED',
        result: { ok: true },
      });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/result',
        { status: 'COMPLETED', result: { ok: true } },
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(resultData);
    });

    it('supports artifacts in submission', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({ data: { taskId: 'task-1' } });

      await client.submitResult('task-1', {
        status: 'COMPLETED',
        result: { ok: true },
        artifacts: [
          { name: 'output.json', contentBase64: 'eyJhIjoxfQ==', contentType: 'application/json' },
        ],
      });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/result',
        expect.objectContaining({
          artifacts: [
            expect.objectContaining({ name: 'output.json' }),
          ],
        }),
        expect.any(Object)
      );
    });

    it('throws if worker token is missing', async () => {
      const client = createClient({ workerToken: undefined });
      await expect(
        client.submitResult('task-1', { status: 'COMPLETED', result: {} })
      ).rejects.toThrow('Worker token is required to submit results');
    });
  });

  // ──────────────────────────────────────────────
  // heartbeat
  // ──────────────────────────────────────────────

  describe('heartbeat', () => {
    it('sends POST with default extendSeconds of 300', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({});

      await client.heartbeat('task-1');

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/heartbeat',
        { extendSeconds: 300 },
        { headers: { Authorization: 'Bearer worker-token' } }
      );
    });

    it('accepts custom extendSeconds', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({});

      await client.heartbeat('task-1', 120);

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/heartbeat',
        { extendSeconds: 120 },
        expect.any(Object)
      );
    });

    it('throws if worker token is missing', async () => {
      const client = createClient({ workerToken: undefined });
      await expect(client.heartbeat('task-1')).rejects.toThrow(
        'Worker token is required for heartbeat'
      );
    });
  });

  // ──────────────────────────────────────────────
  // abandon
  // ──────────────────────────────────────────────

  describe('abandon', () => {
    it('sends POST to abandon endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({});

      await client.abandon('task-1');

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/abandon',
        {},
        { headers: { Authorization: 'Bearer worker-token' } }
      );
    });

    it('throws if worker token is missing', async () => {
      const client = createClient({ workerToken: undefined });
      await expect(client.abandon('task-1')).rejects.toThrow(
        'Worker token is required to abandon tasks'
      );
    });
  });

  // ──────────────────────────────────────────────
  // nack
  // ──────────────────────────────────────────────

  describe('nack', () => {
    it('sends POST with delay and reason and returns NackResponse', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const nackResp = { status: 'requeued', delaySeconds: 30 };
      mock.post.mockResolvedValue({ data: nackResp });

      const result = await client.nack('task-1', 30, 'temp failure');

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/nack',
        { delaySeconds: 30, reason: 'temp failure' },
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(nackResp);
    });

    it('throws if worker token is missing', async () => {
      const client = createClient({ workerToken: undefined });
      await expect(client.nack('task-1', 30, 'reason')).rejects.toThrow(
        'Worker token is required to NACK tasks'
      );
    });
  });

  // ──────────────────────────────────────────────
  // createSubscription
  // ──────────────────────────────────────────────

  describe('createSubscription', () => {
    it('sends POST to subscriptions endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const subResp = { subscriptionId: 'sub-1', expiresAt: '2026-01-01T01:00:00Z' };
      mock.post.mockResolvedValue({ data: subResp });

      const result = await client.createSubscription({
        callbackUrl: 'https://myapp.com/hook',
        eventTypes: ['CMD_A'],
        ttlSeconds: 3600,
        deliveryMode: 'group',
        groupId: 'grp-1',
      });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/workers/subscriptions',
        {
          callbackUrl: 'https://myapp.com/hook',
          eventTypes: ['CMD_A'],
          ttlSeconds: 3600,
          deliveryMode: 'group',
          groupId: 'grp-1',
        },
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(subResp);
    });

    it('throws if worker token is missing', async () => {
      const client = createClient({ workerToken: undefined });
      await expect(
        client.createSubscription({ callbackUrl: 'https://x.com/hook' })
      ).rejects.toThrow('Worker token is required to create subscriptions');
    });
  });

  // ──────────────────────────────────────────────
  // renewSubscription
  // ──────────────────────────────────────────────

  describe('renewSubscription', () => {
    it('sends POST to subscription heartbeat endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const subResp = { subscriptionId: 'sub-1', expiresAt: '2026-01-01T02:00:00Z' };
      mock.post.mockResolvedValue({ data: subResp });

      const result = await client.renewSubscription('sub-1', { ttlSeconds: 7200 });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/workers/subscriptions/sub-1/heartbeat',
        { ttlSeconds: 7200 },
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(subResp);
    });

    it('sends empty body when no options provided', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({ data: { subscriptionId: 'sub-1' } });

      await client.renewSubscription('sub-1');

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/workers/subscriptions/sub-1/heartbeat',
        {},
        expect.any(Object)
      );
    });

    it('falls back to producer token', async () => {
      const client = createClient({ workerToken: undefined });
      const mock = getMockInstance();
      mock.post.mockResolvedValue({ data: { subscriptionId: 'sub-1' } });

      await client.renewSubscription('sub-1');

      expect(mock.post).toHaveBeenCalledWith(
        expect.any(String),
        expect.any(Object),
        { headers: { Authorization: 'Bearer producer-token' } }
      );
    });

    it('throws if no token is available', async () => {
      const client = createClient({ workerToken: undefined, producerToken: undefined });
      await expect(client.renewSubscription('sub-1')).rejects.toThrow(
        'Token is required to renew subscriptions'
      );
    });
  });

  // ──────────────────────────────────────────────
  // getTask
  // ──────────────────────────────────────────────

  describe('getTask', () => {
    it('sends GET to task endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const taskData = { id: 'task-1', status: TaskStatus.PENDING };
      mock.get.mockResolvedValue({ data: taskData });

      const result = await client.getTask('task-1');

      expect(mock.get).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1',
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(taskData);
    });

    it('falls back to producer token', async () => {
      const client = createClient({ workerToken: undefined });
      const mock = getMockInstance();
      mock.get.mockResolvedValue({ data: { id: 'task-1' } });

      await client.getTask('task-1');

      expect(mock.get).toHaveBeenCalledWith(
        expect.any(String),
        { headers: { Authorization: 'Bearer producer-token' } }
      );
    });

    it('throws if no token is available', async () => {
      const client = createClient({ workerToken: undefined, producerToken: undefined });
      await expect(client.getTask('task-1')).rejects.toThrow(
        'Token is required to get task'
      );
    });
  });

  // ──────────────────────────────────────────────
  // getResult
  // ──────────────────────────────────────────────

  describe('getResult', () => {
    it('sends GET to result endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const resultData = {
        task: { id: 'task-1' },
        result: { taskId: 'task-1', status: TaskStatus.COMPLETED },
      };
      mock.get.mockResolvedValue({ data: resultData });

      const result = await client.getResult('task-1');

      expect(mock.get).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task-1/result',
        { headers: { Authorization: 'Bearer worker-token' } }
      );
      expect(result).toEqual(resultData);
    });

    it('throws if no token is available', async () => {
      const client = createClient({ workerToken: undefined, producerToken: undefined });
      await expect(client.getResult('task-1')).rejects.toThrow(
        'Token is required to get result'
      );
    });
  });

  // ──────────────────────────────────────────────
  // waitForResult
  // ──────────────────────────────────────────────

  describe('waitForResult', () => {
    it('returns immediately when result is available', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const resultData = {
        task: { id: 'task-1' },
        result: { taskId: 'task-1', status: TaskStatus.COMPLETED },
      };
      mock.get.mockResolvedValue({ data: resultData });

      const result = await client.waitForResult('task-1', { timeout: 5000 });
      expect(result).toEqual(resultData);
    });

    it('polls until result becomes available', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const resultData = {
        task: { id: 'task-1' },
        result: { taskId: 'task-1', status: TaskStatus.COMPLETED },
      };

      let callCount = 0;
      mock.get.mockImplementation(() => {
        callCount++;
        if (callCount < 3) {
          return Promise.reject(new Error('not ready'));
        }
        return Promise.resolve({ data: resultData });
      });

      const result = await client.waitForResult('task-1', {
        timeout: 5000,
        pollInterval: 10,
      });
      expect(result).toEqual(resultData);
      expect(callCount).toBe(3);
    });

    it('throws timeout error when result is not available in time', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.get.mockRejectedValue(new Error('not ready'));

      await expect(
        client.waitForResult('task-1', { timeout: 50, pollInterval: 10 })
      ).rejects.toThrow('Timed out waiting for result of task task-1 after 50ms');
    });

    it('uses default timeout and pollInterval', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const resultData = {
        task: { id: 'task-1' },
        result: { taskId: 'task-1', status: TaskStatus.COMPLETED },
      };
      mock.get.mockResolvedValue({ data: resultData });

      const result = await client.waitForResult('task-1');
      expect(result).toEqual(resultData);
    });
  });

  // ──────────────────────────────────────────────
  // Admin: listQueues
  // ──────────────────────────────────────────────

  describe('listQueues', () => {
    it('sends GET to admin queues endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const queues = [
        { command: 'CMD_A', ready: 10, delayed: 2, inProgress: 3, dlq: 0 },
      ];
      mock.get.mockResolvedValue({ data: queues });

      const result = await client.listQueues();

      expect(mock.get).toHaveBeenCalledWith(
        '/v1/codeq/admin/queues',
        { headers: { Authorization: 'Bearer admin-token' } }
      );
      expect(result).toEqual(queues);
    });

    it('throws if admin token is missing', async () => {
      const client = createClient({ adminToken: undefined });
      await expect(client.listQueues()).rejects.toThrow(
        'Admin token is required to list queues'
      );
    });
  });

  // ──────────────────────────────────────────────
  // Admin: getQueueStats
  // ──────────────────────────────────────────────

  describe('getQueueStats', () => {
    it('sends GET to admin queue stats endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const stats = { command: 'CMD_A', ready: 10, delayed: 2, inProgress: 3, dlq: 0 };
      mock.get.mockResolvedValue({ data: stats });

      const result = await client.getQueueStats('CMD_A');

      expect(mock.get).toHaveBeenCalledWith(
        '/v1/codeq/admin/queues/CMD_A',
        { headers: { Authorization: 'Bearer admin-token' } }
      );
      expect(result).toEqual(stats);
    });

    it('throws if admin token is missing', async () => {
      const client = createClient({ adminToken: undefined });
      await expect(client.getQueueStats('CMD_A')).rejects.toThrow(
        'Admin token is required to get queue stats'
      );
    });
  });

  // ──────────────────────────────────────────────
  // Admin: cleanupExpired
  // ──────────────────────────────────────────────

  describe('cleanupExpired', () => {
    it('sends POST to admin cleanup endpoint', async () => {
      const client = createClient();
      const mock = getMockInstance();
      const cleanupResp = { deleted: 42, before: '2026-01-01T00:00:00Z', limit: 1000 };
      mock.post.mockResolvedValue({ data: cleanupResp });

      const result = await client.cleanupExpired({ limit: 500 });

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/admin/tasks/cleanup',
        { limit: 500 },
        { headers: { Authorization: 'Bearer admin-token' } }
      );
      expect(result).toEqual(cleanupResp);
    });

    it('sends empty body when no options provided', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.post.mockResolvedValue({ data: { deleted: 0 } });

      await client.cleanupExpired();

      expect(mock.post).toHaveBeenCalledWith(
        '/v1/codeq/admin/tasks/cleanup',
        {},
        expect.any(Object)
      );
    });

    it('throws if admin token is missing', async () => {
      const client = createClient({ adminToken: undefined });
      await expect(client.cleanupExpired()).rejects.toThrow(
        'Admin token is required for cleanup'
      );
    });
  });

  // ──────────────────────────────────────────────
  // URL encoding
  // ──────────────────────────────────────────────

  describe('URL encoding', () => {
    it('encodes task IDs in URL paths', async () => {
      const client = createClient();
      const mock = getMockInstance();
      mock.get.mockResolvedValue({ data: { id: 'task/special' } });

      await client.getTask('task/special');

      expect(mock.get).toHaveBeenCalledWith(
        '/v1/codeq/tasks/task%2Fspecial',
        expect.any(Object)
      );
    });
  });
});
