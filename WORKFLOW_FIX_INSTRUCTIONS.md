# Workflow Fix Instructions

## Issue Summary

The daily-plan workflow (and potentially other agentic workflows) was failing because the agent prepared the issue content but never called the safe output tool to actually create the issue. The conversation ended with the agent saying "Now I'll use the safeoutputs tool..." but never making the actual tool call.

## Root Causes

### 1. Missing Explicit Tool Call Instructions

The workflow prompt instructions in the `.md` files said things like "Create a new issue" without explicitly instructing the agent to **call the MCP tool**. The agent prepared content but didn't understand it needed to invoke the `create_issue` or `create_pull_request` tools from the `safeoutputs` MCP server.

### 2. Remote Source References

**CRITICAL**: All workflows had `source:` lines in their frontmatter pointing to external repositories:
- `source: githubnext/agentics/workflows/daily-plan.md@69b5e3ae5fa7f35fa555b0a22aee14c36ab57ebb`  
- `source: github/gh-aw/.github/workflows/code-simplifier.md@9fd9e56e1c7899f517b12dc9e36022a4e4921093`

This meant the workflows were fetching prompts from upstream repositories at runtime, **completely ignoring local changes**. Even after fixing the local `.md` files, the workflows would still use the old prompts from the external sources.

## Fix Applied

### 1. Updated Prompt Instructions

Updated all agentic workflow `.md` files to explicitly instruct the agent to call the safe output tools:

### 2. Removed Remote Source References

Removed all `source:` lines from workflow frontmatter so that workflows use local content:

**Before:**
```yaml
---
timeout-minutes: 15
source: githubnext/agentics/workflows/daily-plan.md@69b5e3ae5fa7f35fa555b0a22aee14c36ab57ebb
---
```

**After:**
```yaml
---
timeout-minutes: 15
---
```

This change ensures that:
- Workflows read prompts from local `.md` files instead of fetching from external repositories
- Local changes take effect immediately after recompilation
- The repository has full control over workflow behavior

### 3. Fixed Validation Errors

Fixed `add-comment` configuration in `daily-qa.md` and `daily-perf-improver.md`:
- Removed invalid `issue: true` and `discussion: true` properties
- Kept `target: "*"` which properly handles all issues and PRs

### Files Modified:

All workflow `.md` files and their compiled `.lock.yml` counterparts:
1. `.github/workflows/daily-plan.md` - Added explicit `create_issue` tool call instruction
2. `.github/workflows/daily-repo-status.md` - Added explicit `create_issue` tool call instruction  
3. `.github/workflows/daily-qa.md` - Added explicit `create_issue` tool call instruction
4. `.github/workflows/daily-perf-improver.md` - Added explicit `create_pull_request` tool call instruction
5. `.github/workflows/code-simplifier.md` - Added explicit `create_pull_request` tool call instruction
6. `.github/workflows/update-docs.md` - Added explicit `create_pull_request` tool call instruction

### Example Change:

**Before:**
```markdown
3. Create a new planning issue with the project plan in its body.
```

**After:**
```markdown
3. Create a new planning issue with the project plan in its body using the `create_issue` tool.

   3a. **IMPORTANT**: You MUST call the `create_issue` tool (from the safeoutputs MCP server) to create the issue. Simply preparing the content is not sufficient - you must actually invoke the tool.
```

## Required Next Steps

⚠️ **IMPORTANT**: The changes have been compiled and committed. To deploy:

1. **Merge this PR** to activate the fixes
2. **Manually trigger the daily-plan workflow** to test: https://github.com/osvaldoandrade/codeq/actions/workflows/daily-plan.lock.yml
3. **Monitor the workflow run** to ensure an issue is created successfully

No additional compilation is needed - the `.lock.yml` files have been updated.

### Verification

After merging and triggering a workflow run:

1. **Check workflow logs**: Verify the agent receives the updated prompt with "IMPORTANT" instructions
2. **Check issue creation**: Verify that an issue is created with title starting with "Agentic Planner" or appropriate workflow name  
3. **No failure issues**: Verify that no "No Safe Outputs Generated" failure issue is created
4. **Workflow completes successfully**: Verify green checkmark on the workflow run

You can also verify locally before merging:

```bash
# Verify the IMPORTANT instruction is in the .md file
grep "IMPORTANT.*call.*create" .github/workflows/daily-plan.md

# Verify no external source reference
! grep "^source:.*githubnext\|^source:.*github/gh-aw" .github/workflows/*.md

# Verify frontmatter-hash changed in lock file (indicates recompilation)
git log -p -1 .github/workflows/daily-plan.lock.yml | grep frontmatter-hash
```

## Testing

To test the fix:

1. Run the daily-plan workflow manually: `https://github.com/<OWNER>/<REPO>/actions/workflows/daily-plan.lock.yml`
2. Wait for it to complete
3. Verify that:
   - An issue is created with title starting with "Agentic Planner"
   - No "No Safe Outputs Generated" failure issue is created
   - The workflow completes successfully

## Key Learning

**Always check for `source:` references in workflow frontmatter!**

When workflows have `source:` lines pointing to external repositories:
- The local `.md` file content is **ignored at runtime**
- Prompts are fetched from the external source
- Local changes have **no effect** until the source reference is removed
- This is a common source of confusion when debugging workflow issues

The proper workflow development process:
1. Remove `source:` references for local development
2. Edit the `.md` file with your changes
3. Compile with `gh aw compile`
4. Test the workflow
5. Only add `source:` references if you want to sync with an upstream template

For future workflow development:
- Always explicitly instruct the agent to **call the MCP tool** when safe outputs are expected
- Include the tool name (e.g., `create_issue`, `create_pull_request`)  
- Mention the MCP server name (`safeoutputs`)
- Emphasize that preparing content without calling the tool is insufficient
- Test workflows thoroughly before deploying to production

## Related Issue

This fix addresses issue #146: [agentics] Agentic Planner failed
- Workflow Run: https://github.com/osvaldoandrade/codeq/actions/runs/22051897101
- Symptom: "No Safe Outputs Generated" 
- Root Cause: Agent didn't call the safe output tool
