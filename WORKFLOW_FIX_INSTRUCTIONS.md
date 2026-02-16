# Workflow Fix Instructions

## Issue Summary

The daily-plan workflow (and potentially other agentic workflows) was failing because the agent prepared the issue content but never called the safe output tool to actually create the issue. The conversation ended with the agent saying "Now I'll use the safeoutputs tool..." but never making the actual tool call.

## Root Cause

The workflow prompt instructions in the `.md` files said things like "Create a new issue" without explicitly instructing the agent to **call the MCP tool**. The agent prepared content but didn't understand it needed to invoke the `create_issue` or `create_pull_request` tools from the `safeoutputs` MCP server.

## Fix Applied

Updated all agentic workflow `.md` files to explicitly instruct the agent to call the safe output tools:

### Files Modified:
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

⚠️ **IMPORTANT**: The `.md` files need to be compiled into `.lock.yml` files using the gh-aw CLI:

```bash
# Install gh-aw extension if not already installed
gh extension install github/gh-aw

# Compile all workflow files
gh aw compile

# Or compile specific workflow
gh aw compile .github/workflows/daily-plan.md
```

The `.lock.yml` files are the actual workflow files that GitHub Actions executes. The `.md` files are the source, and they must be compiled.

### Verification

After compiling, verify the changes made it into the lock files:

```bash
# Check that frontmatter-hash changed in lock file
git diff .github/workflows/daily-plan.lock.yml | grep frontmatter-hash

# Check that the IMPORTANT instruction is present in the compiled prompt
grep -A 5 "IMPORTANT.*create_issue" .github/workflows/daily-plan.lock.yml
```

## Testing

To test the fix:

1. Run the daily-plan workflow manually: https://github.com/osvaldoandrade/codeq/actions/workflows/daily-plan.lock.yml
2. Wait for it to complete
3. Verify that:
   - An issue is created with title starting with "Agentic Planner"
   - No "No Safe Outputs Generated" failure issue is created
   - The workflow completes successfully

## Prevention

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
