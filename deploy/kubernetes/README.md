# Kubernetes Deployment

Use the Helm chart in `helm/codeq` for production codeQ server installs.

```bash
codeq install --target kubernetes --size small
```

The raw manifests in this directory are framework example deployments for
applications that talk to codeQ. They are not the primary production install
path for the codeQ server itself.
