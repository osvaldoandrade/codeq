# CLI Reference

The `codeq` CLI is a command-line interface for interacting with the codeQ task scheduling system. It provides commands for producers, workers, and operators.

## Installation

### Via npm (recommended)

````bash
npm i -g @osvaldoandrade/codeq
codeq --help
````

### Via install script

````bash
curl -fsSL https://raw.githubusercontent.com/osvaldoandrade/codeq/main/install.sh | sh
````

Requires `git` and `go`.

### From source

````bash
git clone https://github.com/osvaldoandrade/codeq
cd codeq
go build -o codeq ./cmd/codeq
./codeq --help
````

## Configuration

The CLI stores configuration in `~/.codeq/config.yaml`.

### Profiles

The CLI supports multiple profiles for different environments (dev, staging, prod):

````yaml
currentProfile: dev
profiles:
  dev:
    baseUrl: http://localhost:8080
    iamBaseUrl: https://api.storifly.ai/v1/accounts
    iamApiKey: your-api-key
    producerToken: dev-producer-token
    workerToken: dev-worker-token
    admin: true
  prod:
    baseUrl: https://codeq.example.com
    producerToken: prod-producer-token
    workerToken: prod-worker-token
    admin: false
````

Switch profiles with `--profile`:

````bash
codeq --profile prod task create --event MY_EVENT
````

### Environment Variables

Override config with environment variables:

- `CODEQ_BASE_URL`: Base URL for codeQ API
- `CODEQ_PRODUCER_TOKEN`: Producer authentication token
- `CODEQ_WORKER_TOKEN`: Worker JWT token
- `CODEQ_ADMIN`: Send `X-Role: ADMIN` header (dev only)
- `CODEQ_PROFILE`: Active profile name
- `CODEQ_IAM_BASE_URL`: IAM service base URL
- `CODEQ_IAM_API_KEY`: IAM API key

## Global Flags

These flags apply to all commands:

- `--base-url <url>`: Base URL for codeQ API
- `--producer-token <token>`: Producer authentication token
- `--worker-token <token>`: Worker JWT token
- `--admin`: Send X-Role: ADMIN header (dev/local only)
- `--profile <name>`: Use specific config profile
- `--iam-base-url <url>`: IAM service base URL
- `--iam-api-key <key>`: IAM API key

## Commands

### codeq init

Initialize or update CLI configuration interactively.

````bash
codeq init
````

**Options:**
- `--base-url <url>`: Base URL
- `--iam-base-url <url>`: IAM base URL
- `--iam-api-key <key>`: IAM API key
- `--producer-token <token>`: Producer token
- `--worker-token <token>`: Worker token
- `--admin`: Enable admin mode
- `--no-prompt`: Skip interactive prompts

**Example:**

````bash
codeq init --base-url http://localhost:8080 --admin
````

---

### codeq auth

Manage authentication credentials.

#### codeq auth login

Login via IAM, exchange idToken for accessToken, and store it.

````bash
codeq auth login
````

**Options:**
- `--save`: Save login/exchange config (default: true)
- `--no-prompt`: Disable interactive prompts

**How it works:**

1. Opens browser to IAM login page
2. User authenticates and gets an `idToken`
3. CLI exchanges `idToken` for `accessToken` via IAM API
4. Stores `accessToken` as producer token

#### codeq auth set

Store tokens directly in config.

````bash
codeq auth set --producer <token>
codeq auth set --worker <token>
codeq auth set --producer <token> --worker <token>
````

**Options:**
- `--producer <token>`: Store producer token
- `--worker <token>`: Store worker token

#### codeq auth show

Display stored credentials (masked).

````bash
codeq auth show
````

Shows current profile configuration with tokens partially masked.

#### codeq auth clear

Clear stored tokens.

````bash
codeq auth clear              # Clear all tokens
codeq auth clear --producer   # Clear producer token only
codeq auth clear --worker     # Clear worker token only
codeq auth clear --all        # Clear all tokens (explicit)
````

**Options:**
- `--producer`: Clear producer token
- `--worker`: Clear worker token
- `--all`: Clear all tokens

---

### codeq task

Task operations for producers.

#### codeq task create

Create a new task.

````bash
codeq task create --event <event> [options]
````

**Required:**
- `--event <string>`: Event/command type (e.g., `GENERATE_MASTER`)

**Options:**
- `--payload <json>`: Task payload as JSON string (default: `{}`)
- `--priority <int>`: Task priority, higher = more urgent (default: 5)
- `--webhook <url>`: Callback URL for result notification
- `--max-attempts <int>`: Maximum retry attempts (default: 3)
- `--idempotency-key <string>`: Idempotency key for duplicate prevention
- `--run-at <timestamp>`: Schedule task for specific time (ISO 8601)
- `--delay <seconds>`: Delay task execution by N seconds

**Examples:**

Create immediate task:
````bash
codeq task create \
  --event GENERATE_MASTER \
  --payload '{"jobId":"j-123","output":"hd"}' \
  --priority 10
````

Create scheduled task:
````bash
codeq task create \
  --event GENERATE_MASTER \
  --payload '{"jobId":"j-456"}' \
  --run-at "2026-02-20T15:00:00Z"
````

Create delayed task:
````bash
codeq task create \
  --event SEND_EMAIL \
  --payload '{"to":"user@example.com"}' \
  --delay 3600
````

With callback webhook:
````bash
codeq task create \
  --event PROCESS_VIDEO \
  --payload '{"videoId":"v-789"}' \
  --webhook "https://api.example.com/callbacks/codeq"
````

#### codeq task get

Get task details by ID.

````bash
codeq task get <task-id>
````

**Example:**

````bash
codeq task get 01HWXYZ1234567890ABCDEFGH
````

**Output:**

````json
{
  "id": "01HWXYZ1234567890ABCDEFGH",
  "command": "GENERATE_MASTER",
  "payload": "{\"jobId\":\"j-123\"}",
  "status": "COMPLETED",
  "priority": 10,
  "createdAt": "2026-02-14T10:00:00Z",
  "completedAt": "2026-02-14T10:05:30Z"
}
````

#### codeq task result

Get task result by task ID.

````bash
codeq task result <task-id>
````

**Example:**

````bash
codeq task result 01HWXYZ1234567890ABCDEFGH
````

**Output:**

````json
{
  "taskId": "01HWXYZ1234567890ABCDEFGH",
  "status": "COMPLETED",
  "result": {
    "outputUrl": "https://cdn.example.com/output.mp4",
    "duration": 125.5
  },
  "completedAt": "2026-02-14T10:05:30Z"
}
````

---

### codeq worker

Worker operations for task processing.

#### codeq worker start

Start a worker that polls and processes tasks.

````bash
codeq worker start --events <event1,event2> [options]
````

**Required:**
- `--events <list>`: Comma-separated list of event types to process

**Options:**
- `--concurrency <int>`: Number of concurrent tasks to process (default: 1)
- `--handler <path>`: Path to handler script (default: `./handler.sh`)
- `--lease <seconds>`: Lease duration for claimed tasks (default: 300)
- `--poll <seconds>`: Long-poll wait time (default: 30)
- `--once`: Process one task and exit

**Handler Script:**

The handler script receives task data via stdin and should output JSON result:

````bash
#!/bin/bash
# handler.sh

# Read task data
TASK=$(cat)
TASK_ID=$(echo "$TASK" | jq -r '.id')
PAYLOAD=$(echo "$TASK" | jq -r '.payload')

# Process task
echo "Processing task $TASK_ID" >&2

# Return result as JSON
echo '{"status":"COMPLETED","result":{"processed":true}}'
````

**Examples:**

Start worker for single event:
````bash
codeq worker start --events GENERATE_MASTER --concurrency 5
````

Start worker for multiple events:
````bash
codeq worker start \
  --events GENERATE_MASTER,RENDER_VIDEO,SEND_EMAIL \
  --concurrency 10 \
  --handler ./my-handler.sh
````

Process one task and exit:
````bash
codeq worker start --events GENERATE_MASTER --once
````

---

### codeq queue

Queue operations for operators.

#### codeq queue inspect

Inspect queue depth and statistics for an event type.

````bash
codeq queue inspect <event>
````

**Example:**

````bash
codeq queue inspect GENERATE_MASTER
````

**Output:**

````json
{
  "command": "GENERATE_MASTER",
  "ready": 42,
  "delayed": 8,
  "inProgress": 15,
  "dlq": 2
}
````

**Fields:**
- `ready`: Tasks ready to be claimed
- `delayed`: Tasks scheduled for future execution
- `inProgress`: Tasks currently claimed by workers
- `dlq`: Tasks in dead-letter queue (exceeded max attempts)

---

## Exit Codes

- `0`: Success
- `1`: General error
- `2`: Configuration error
- `3`: Authentication error
- `4`: API error
- `130`: Interrupted (Ctrl+C)

## Troubleshooting

### Authentication Issues

**Problem**: `401 Unauthorized`

**Solution**: Verify your tokens are valid:

````bash
codeq auth show
````

Re-login if needed:

````bash
codeq auth login
````

### Connection Issues

**Problem**: `connection refused` or `timeout`

**Solution**: Check base URL is correct:

````bash
codeq --base-url http://localhost:8080 task get <id>
````

Verify the codeQ service is running.

### Configuration Issues

**Problem**: CLI not finding config

**Solution**: Initialize configuration:

````bash
codeq init
````

Or specify values via environment variables:

````bash
export CODEQ_BASE_URL=http://localhost:8080
export CODEQ_PRODUCER_TOKEN=your-token
codeq task create --event TEST
````

## Advanced Usage

### Scripting and Automation

Use JSON output for scripting:

````bash
# Create task and capture ID
TASK_ID=$(codeq task create \
  --event PROCESS \
  --payload '{"data":"value"}' \
  | jq -r '.id')

# Poll for completion
while true; do
  STATUS=$(codeq task get "$TASK_ID" | jq -r '.status')
  if [ "$STATUS" = "COMPLETED" ]; then
    codeq task result "$TASK_ID"
    break
  fi
  sleep 5
done
````

### Custom Worker Handlers

Handler script examples:

**Python handler:**

````python
#!/usr/bin/env python3
import json
import sys

# Read task from stdin
task = json.load(sys.stdin)
task_id = task['id']
payload = json.loads(task['payload'])

# Process task
result = process_task(payload)

# Output result
print(json.dumps({
    "status": "COMPLETED",
    "result": result
}))
````

**Node.js handler:**

````javascript
#!/usr/bin/env node
const readline = require('readline');

const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false
});

let input = '';
rl.on('line', line => { input += line; });

rl.on('close', async () => {
  const task = JSON.parse(input);
  const payload = JSON.parse(task.payload);
  
  // Process task
  const result = await processTask(payload);
  
  // Output result
  console.log(JSON.stringify({
    status: 'COMPLETED',
    result: result
  }));
});
````

## See Also

- [HTTP API Documentation](./04-http-api.md)
- [Configuration Guide](./14-configuration.md)
- [Worker Examples](./13-examples.md)
- [Security](./09-security.md)
