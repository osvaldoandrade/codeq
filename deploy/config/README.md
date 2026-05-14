# Configuration Examples

`codeq.example.yml` is the canonical file-based configuration example for the
server. It mirrors the environment variables documented in
`docs/14-configuration.md`.

Use it as a starting point for non-containerized deployments:

```bash
cp deploy/config/codeq.example.yml ./codeq.yml
```

For Docker Compose and Helm deployments, prefer the templates under
`deploy/docker-compose` and `helm/codeq`; those paths inject configuration
through environment variables or chart values.
