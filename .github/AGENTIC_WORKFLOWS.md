# Agentic Workflows

This document explains the agentic workflows configured in this repository and how they operate.

## What are Agentic Workflows?

Agentic workflows are AI-powered GitHub Actions workflows that autonomously perform repository maintenance tasks such as:

- Documentation updates
- Code quality improvements
- Performance optimizations
- Project planning
- Repository health monitoring

These workflows use the `gh-aw` (GitHub Agentic Workflows) framework.

## Configured Workflows

### Scheduled (Daily) Workflows

These workflows run on a daily schedule and check if any work is needed:

- **Daily Plan** (`daily-plan.lock.yml`) - Maintains project roadmap and planning
- **Daily QA** (`daily-qa.lock.yml`) - Automated code quality assurance
- **Daily Performance Improver** (`daily-perf-improver.lock.yml`) - Identifies performance bottlenecks
- **Daily Repo Status** (`daily-repo-status.lock.yml`) - Generates repository health reports
- **Code Simplifier** (`code-simplifier.lock.yml`) - Identifies overly complex code

### Event-Triggered Workflows

These workflows run in response to repository events:

- **Update Docs** (`update-docs.lock.yml`) - Keeps documentation synchronized with code changes
  - Triggers: Push to main (with path filters)
  - Optimized to only run when relevant files change

## No-Op Runs and Tracking

### What is a No-Op Run?

A "no-op" (no operation) run occurs when a workflow executes but determines no action is needed. For example:

- A documentation workflow runs but finds all docs are already up-to-date
- A planning workflow runs but determines the current plan is still accurate
- A QA workflow runs but finds no quality issues to address

### No-Op Tracking Issue

All agentic workflows are configured to report no-op completions to a tracking issue titled **"[agentics] No-Op Runs"**. This issue:

- Collects all no-op workflow run reports in one place
- Helps identify optimization opportunities
- Is automatically managed by the workflows
- Should not be closed manually

### Why Track No-Ops?

While no-op runs are harmless and often expected (especially for scheduled workflows), tracking them helps:

1. **Resource optimization**: Identify workflows that frequently produce no-ops and could benefit from better triggers
2. **Configuration validation**: Ensure workflows are running at appropriate times
3. **Cost management**: Monitor CI/CD resource consumption

### Reducing No-Op Runs

#### Path Filters (Event-Triggered Workflows)

Workflows triggered by repository events (like `update-docs`) use path filters to avoid unnecessary runs:

```yaml
on:
  push:
    branches: [main]
    paths:
      - '**.go'          # Only run when code changes
      - 'docs/**'        # Or documentation changes
      - 'go.mod'         # Or dependencies change
      # etc.
```

This prevents the workflow from running when unrelated files (like wiki pages) change.

#### Skip-If-Match (Pre-activation Checks)

Some workflows check for existing work before activating:

```yaml
skip-if-match: is:pr is:open in:title "[workflow-name]"
```

This prevents duplicate work if a PR is already open.

#### Smart Workflow Logic

Workflows are designed to:
- Analyze the current state before taking action
- Exit early if no work is needed
- Report no-op status rather than failing

### Expected No-Ops

No-ops from **scheduled workflows** are normal and expected. For example:

- Daily plan runs but the project plan is already current
- Daily QA runs but no new code quality issues exist
- Performance improver runs but no obvious optimizations are available

These no-ops indicate the workflows are functioning correctly.

### Unexpected No-Ops

No-ops from **event-triggered workflows** may indicate:

- Path filters are too broad (workflow runs on irrelevant changes)
- Skip conditions are not working correctly
- Workflow logic needs refinement

If you notice frequent no-ops from event-triggered workflows, consider:

1. Reviewing the path filters in the workflow definition
2. Adding more specific skip conditions
3. Adjusting the workflow trigger conditions

## Workflow Maintenance

### Updating Workflow Definitions

Workflow definitions are stored as Markdown files (`.md`) in `.github/workflows/` and compiled to YAML (`.lock.yml`):

1. Edit the `.md` source file (e.g., `update-docs.md`)
2. Run `gh aw compile` to generate the `.lock.yml` file
3. Commit both files

**Note**: The `.lock.yml` files are auto-generated and should not be manually edited (except for hotfixes, which should be documented).

### Monitoring Workflow Runs

- View workflow runs in the **Actions** tab
- Check the no-op tracking issue for recent no-op reports
- Monitor CI/CD resource usage in repository settings

### Disabling Workflows

If a workflow is causing issues or creating too much noise:

1. Temporarily disable it in repository settings (Actions tab)
2. Or delete/rename the `.lock.yml` file to prevent execution
3. Address the underlying issue
4. Re-enable when ready

## See Also

- [Workflows Documentation](../docs/16-workflows.md) - Detailed workflow descriptions
- [Contributing Guide](../CONTRIBUTING.md) - How to contribute to this repository
- [GitHub Actions Documentation](https://docs.github.com/en/actions) - Official GitHub Actions docs
