# Usage examples

## Producer: create task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks \
  -H 'Authorization: Bearer <producer-token>' \
  -H 'Content-Type: application/json' \
  -d '{"command":"GENERATE_MASTER","payload":{"jobId":"j-123"},"priority":3}'
```

## Worker: claim task

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/claim \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"commands":["GENERATE_MASTER"],"leaseSeconds":120,"waitSeconds":10}'
```

## Worker: submit result

```bash
curl -X POST http://localhost:8080/v1/codeq/tasks/<id>/result \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"status":"COMPLETED","result":{"ok":true}}'
```

## Worker: register webhook

```bash
curl -X POST http://localhost:8080/v1/codeq/workers/subscriptions \
  -H 'Authorization: Bearer <worker-token>' \
  -H 'Content-Type: application/json' \
  -d '{"eventTypes":["GENERATE_MASTER"],"callbackUrl":"https://worker.example.org/codeq/notify","ttlSeconds":300}'
```
