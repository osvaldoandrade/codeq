# GitHub Workflows

This document describes the automated workflows configured for the codeQ repository.

## Overview

The repository uses GitHub Actions for CI/CD, documentation, code quality, and release automation. Workflows are defined in `.github/workflows/`.

## Workflows

### Release (`release.yml`)

**Trigger**: Push to tags matching `v*.*.*` (e.g., `v0.2.3`)

**Purpose**: Build and publish releases of the codeQ CLI

**Steps**:

1. **Test**: Runs `go test ./cmd/codeq/...` to test CLI code
   - Only tests CLI to avoid private server-side dependencies
   - See [WORKFLOW_FAILURE_ANALYSIS.md](../WORKFLOW_FAILURE_ANALYSIS.md) for background

2. **Build binaries**: Compiles cross-platform binaries
   - Platforms: `linux`, `darwin`, `windows`
   - Architectures: `amd64`, `arm64`
   - Output: `dist/codeq-{os}-{arch}[.exe]`

3. **Checksums**: Generates `SHA256SUMS.txt` for verification

4. **GitHub Release**: Creates release with:
   - Auto-generated release notes
   - Binary attachments
   - Checksums file

5. **NPM Publish**: Publishes `@osvaldoandrade/codeq` to npmjs.org
   - Requires `NPM_TOKEN` secret
   - Version extracted from tag (e.g., `v0.2.3` → `0.2.3`)

**Concurrency**: One release at a time per ref

**Permissions**: `contents: write`

**Example**: Triggered automatically when you push tag `v0.3.0`

---

### Cut Release (`cut-release.yml`)

**Trigger**: Manual workflow dispatch

**Purpose**: Create and push a version tag to trigger the release workflow

**Inputs**:
- `version`: Semantic version (e.g., `0.2.4`)

**Steps**:

1. **Validate version**: Ensures format matches `X.Y.Z`
2. **Create tag**: Creates tag `vX.Y.Z`
3. **Push tag**: Pushes to repository, triggering `release.yml`

**Usage**:

1. Go to Actions → Cut Release
2. Click "Run workflow"
3. Enter version (e.g., `0.2.4`)
4. Click "Run workflow"

**Concurrency**: One cut-release at a time per ref

**Permissions**: `contents: write`

---

### Deploy Static Docs to Pages (`static.yml`)

**Trigger**:
- Push to `main` branch
- Manual workflow dispatch

**Purpose**: Deploy documentation to GitHub Pages

**Steps**:

1. **Build static site**:
   - Copies `index.html` to `public/`
   - Syncs `wiki/` to `public/wiki/`
   - Excludes: `.git/`, `.DS_Store`, `publish.sh`
   - Creates `.nojekyll` for SPA routing

2. **Upload artifact**: Prepares site for Pages deployment

3. **Deploy**: Publishes to GitHub Pages

**Output**: Documentation site at `https://osvaldoandrade.github.io/codeq/`

**Concurrency**: One deployment at a time, skips queued runs

**Permissions**: `contents: read`, `pages: write`, `id-token: write`

**Note**: Requires GitHub Pages to be enabled in repository settings

---

### Update Docs (`update-docs.lock.yml`)

**Trigger**:
- Push to `main` branch
- Manual workflow dispatch

**Purpose**: Autonomous documentation maintenance

**Description**: 

Agentic workflow that:
- Analyzes code changes in main branch
- Identifies documentation gaps
- Creates/updates documentation following best practices
- Opens draft pull requests with documentation changes

**Style Guidelines**:
- Diátaxis framework (tutorials, how-to, reference, explanation)
- Active voice, plain English
- Progressive disclosure
- Accessible and i18n-ready

**Behavior**:
- Creates draft PRs with `automation` and `documentation` labels
- Never pushes directly to main
- Exits if no documentation updates needed

**Timeout**: 15 minutes

**Permissions**: `read-all`

**Tools**: GitHub API, web-fetch, bash

---

### Code Simplifier (`code-simplifier.lock.yml`)

**Trigger**: Manual workflow dispatch

**Purpose**: Identify and simplify overly complex code

**Description**:

Agentic workflow that:
- Analyzes codebase for complexity
- Identifies functions/modules that could be simplified
- Suggests refactoring opportunities
- Creates pull requests with simplifications

**Creates PRs with**: `automation`, `refactoring` labels

**Timeout**: 20 minutes

---

### Daily Performance Improver (`daily-perf-improver.lock.yml`)

**Trigger**:
- Daily schedule: `0 2 * * *` (2 AM UTC)
- Manual workflow dispatch

**Purpose**: Identify and optimize performance bottlenecks

**Description**:

Agentic workflow that:
- Scans code for performance issues
- Checks for inefficient algorithms or data structures
- Suggests optimizations
- Creates pull requests with performance improvements

**Creates PRs with**: `automation`, `performance` labels

**Timeout**: 20 minutes

---

### Daily Plan (`daily-plan.lock.yml`)

**Trigger**:
- Daily schedule: `0 8 * * *` (8 AM UTC)
- Manual workflow dispatch

**Purpose**: Generate daily development plan and priorities

**Description**:

Agentic workflow that:
- Reviews open issues and pull requests
- Analyzes recent activity
- Generates prioritized task list
- Posts plan summary

**Timeout**: 10 minutes

---

### Daily QA (`daily-qa.lock.yml`)

**Trigger**:
- Daily schedule: `0 3 * * *` (3 AM UTC)
- Manual workflow dispatch

**Purpose**: Automated code quality assurance

**Description**:

Agentic workflow that:
- Reviews recent code changes
- Checks for code quality issues
- Identifies potential bugs
- Suggests improvements
- Creates issues or PRs for findings

**Creates PRs with**: `automation`, `quality` labels

**Timeout**: 20 minutes

---

### Daily Repo Status (`daily-repo-status.lock.yml`)

**Trigger**:
- Daily schedule: `0 9 * * *` (9 AM UTC)
- Manual workflow dispatch

**Purpose**: Generate repository health report

**Description**:

Agentic workflow that:
- Analyzes repository metrics
- Reviews open issues and PRs
- Checks CI/CD health
- Generates status report
- Posts summary

**Timeout**: 10 minutes

---

## Workflow Configuration

### Secrets Required

Configure these secrets in repository settings (Settings → Secrets and variables → Actions):

- `NPM_TOKEN`: NPM authentication token for publishing packages
  - Required for `release.yml`
  - Get from: https://www.npmjs.com/settings/YOUR_USERNAME/tokens
  - Type: Automation token

### GitHub Pages Setup

To enable static documentation:

1. Go to Settings → Pages
2. Source: "GitHub Actions"
3. Save

Static site will deploy to: `https://osvaldoandrade.github.io/codeq/`

### Permissions

Workflows use fine-grained permissions following least-privilege principle:

- Most workflows: `read-all` (agentic workflows)
- Release workflows: `contents: write`
- Pages deployment: `pages: write`, `id-token: write`

## Maintenance

### Adding a New Workflow

1. Create workflow file in `.github/workflows/`
2. Test with workflow dispatch trigger first
3. Add to this documentation
4. Consider adding to repository status checks

### Debugging Workflows

View workflow runs:
- Actions tab → Select workflow → Select run
- Download logs for detailed debugging
- Use `act` for local testing: https://github.com/nektos/act

### Workflow Dependencies

Some workflows depend on others:

````
cut-release.yml → (creates tag) → release.yml
release.yml → (publishes) → NPM Registry
static.yml → (deploys) → GitHub Pages
````

### Cost Considerations

Agentic workflows consume GitHub Actions minutes:

- Daily workflows: ~4 runs/day × ~15 min = ~60 min/day
- On-push workflows: Varies by commit frequency
- Manual workflows: Only when triggered

Monitor usage: Settings → Billing and plans → Plans and usage

## Troubleshooting

### Release Workflow Fails

**Common issues**:

1. **NPM_TOKEN missing or invalid**
   - Check: Repository secrets
   - Fix: Add/update NPM_TOKEN secret

2. **Tests fail on private dependencies**
   - Check: Test command only tests `./cmd/codeq/...`
   - Fix: Don't test `./internal/...` or `./pkg/app/...` in release

3. **Tag already exists**
   - Check: GitHub releases and tags
   - Fix: Use a new version number

**Solution**: See [WORKFLOW_FAILURE_ANALYSIS.md](../WORKFLOW_FAILURE_ANALYSIS.md)

### Pages Deployment Fails

**Common issues**:

1. **Pages not enabled**
   - Fix: Enable in Settings → Pages

2. **Permissions error**
   - Check: Workflow has `pages: write` permission
   - Fix: Update workflow file

3. **Build errors**
   - Check: Workflow logs for rsync or file errors
   - Fix: Ensure wiki/ directory exists and is accessible

### Agentic Workflows

**If workflows create too many PRs**:
- Review and close unnecessary PRs
- Adjust workflow frequency (edit schedule)
- Temporarily disable workflow if needed

**If workflows fail to create PRs**:
- Check: Workflow has necessary permissions
- Check: GitHub API rate limits not exceeded
- Review: Workflow logs for specific errors

## See Also

- [Contributing Guide](../CONTRIBUTING.md)
- [Workflow Failure Analysis](../WORKFLOW_FAILURE_ANALYSIS.md)
- [GitHub Actions Documentation](https://docs.github.com/en/actions)
