# Missing Issues for Daily Status Report

This directory contains issue templates for executing the plan outlined in the [Daily Status Report - February 16, 2026](https://github.com/osvaldoandrade/codeq/issues/158).

## Quick Start

To create all 6 missing issues at once, run:

```bash
./create-missing-issues.sh
```

Or test without creating issues:

```bash
./create-missing-issues.sh --dry-run
```

## Why These Issues?

The daily status report identified several recommendations without corresponding tracking issues:

1. **SDK Planning** - "Consider creating detailed specs for Python/TypeScript SDKs"
   - Issues #35 and #36 exist for implementation, but no design specs

2. **Load Testing** - "Try the Load Testing Framework - Run the new k6 benchmarks and share results"
   - No issue to collect and track results

3. **Queue Sharding** - "Review the HLD document in docs/24-queue-sharding-hld.md and provide feedback"
   - HLD exists but no feedback collection issue

4. **Documentation** - "Check out the comprehensive docs and suggest improvements"
   - No structured tracking for improvements

5. **Plugin Architecture** - HLD in docs/25-plugin-architecture-hld.md but no implementation issue

## Issue Details

### 1. Python SDK Specification and Design Document
- **Labels:** enhancement, documentation, sdk, python
- **Purpose:** Create detailed spec before implementing #35
- **Template:** `.github/issue-bodies/python-sdk-spec.md`

### 2. JavaScript/TypeScript SDK Specification and Design Document  
- **Labels:** enhancement, documentation, sdk, javascript, typescript
- **Purpose:** Create detailed spec before implementing #36
- **Template:** `.github/issue-bodies/js-sdk-spec.md`

### 3. Load Testing Baseline Results Collection and Tracking
- **Labels:** performance, testing, help wanted, good first issue
- **Purpose:** Collect baseline metrics from the new k6 framework
- **Template:** `.github/issue-bodies/load-testing-baselines.md`

### 4. Queue Sharding HLD: Feedback Collection and Discussion
- **Labels:** design, discussion, scalability, queue-sharding
- **Purpose:** Gather feedback on docs/24-queue-sharding-hld.md before implementation
- **Template:** `.github/issue-bodies/sharding-hld-feedback.md`

### 5. Documentation Audit and Continuous Improvement Tracking
- **Labels:** documentation, help wanted, good first issue
- **Purpose:** Structured process for documentation quality
- **Template:** `.github/issue-bodies/documentation-audit.md`

### 6. Plugin Architecture Implementation - Phase 1
- **Labels:** enhancement, architecture, plugins, p2
- **Purpose:** Begin implementing docs/25-plugin-architecture-hld.md
- **Template:** `.github/issue-bodies/plugin-architecture-phase1.md`

## Manual Creation

If you prefer to create issues individually:

```bash
# Example: Create Python SDK spec issue
gh issue create \
  --title "Python SDK Specification and Design Document" \
  --label "enhancement,documentation,sdk,python" \
  --body-file .github/issue-bodies/python-sdk-spec.md
```

Repeat for each issue using the appropriate title, labels, and body file.

## Files

- `MISSING_ISSUES.md` - Complete documentation with full issue bodies
- `create-missing-issues.sh` - Script to create all issues at once
- `.github/issue-bodies/*.md` - Individual issue body templates
- `.github/issue-bodies/README.md` - This file

## Related

- Issue #158: [repo-status] Daily Status Report - February 16, 2026
- All issue templates follow the patterns and style of existing codeQ issues
