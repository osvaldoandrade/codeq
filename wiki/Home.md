# codeQ

Start with [Get Started](Get-Started).

codeQ is a reactive scheduling and completion service built on persistent queues in KVRocks. Producers enqueue tasks under a `command` (event type). Workers pull tasks by command, claim ownership via a lease, and then either complete, fail, or nack the task for a delayed retry.

If you are evaluating the system, the fastest reading path is:

1. [Get Started](Get-Started)
2. [Overview](Overview)
3. [Architecture](Architecture)
4. [HTTP API](HTTP-API)
5. [Webhooks](Webhooks)
