# GitHub Discussions Category Recommendation for codeQ

## Summary

Based on the nature of codeQ as a technical infrastructure project with significant architectural discussions (like the queue sharding HLD/RFC), we recommend the following category structure:

## Recommended Category: 🏗️ Architecture & RFCs

For discussions like the **Queue Sharding HLD/RFC**, the best category is a new **Architecture & RFCs** category (if available) or **Ideas** category (as fallback).

### Why This Category?

Architectural discussions like queue sharding have unique needs:

1. **Technical depth**: Require detailed technical analysis, not just feature requests
2. **Multiple options**: Need to compare alternatives (vertical scaling, master-replica, RAFT)
3. **Long-form**: Discussions span weeks and lead to formal documents
4. **Community input**: Benefit from diverse perspectives before implementation
5. **Reference value**: Serve as historical record for design decisions

### What Makes Architecture Discussions Different

| Feature Request (Ideas) | Architecture Discussion (RFCs) |
|------------------------|-------------------------------|
| Single feature or improvement | System-wide change |
| Can be implemented directly | Requires design document first |
| Days to decide | Weeks to months of discussion |
| Implementation details in issue | Design details in docs/ |
| Examples: "Add batch API", "Support PostgreSQL" | Examples: "Queue sharding", "RAFT consensus", "Multi-region active-active" |

## Category Mapping for Common Discussion Types

Given the standard GitHub Discussions categories:

### 📣 Announcements
**Use for**: Release notes, breaking changes, security advisories
**Example**: "codeQ v1.2.0 Released with Queue Sharding"

### 💬 General  
**Use for**: Use cases, success stories, best practices, open-ended conversations
**Example**: "How we scaled codeQ to 100K tasks/minute"

### 💡 Ideas
**Use for**: Feature requests AND architecture proposals (if no RFC category exists)
**Architectural example**: "RFC: Queue Sharding for Horizontal Scaling"
**Feature example**: "Add task dependency support"

### 🗳️ Polls
**Use for**: Community votes on prioritization, naming, defaults
**Example**: "Which sharding strategy: hash, range, or explicit?"

### 🙏 Q&A
**Use for**: How-to questions, troubleshooting, configuration help
**Example**: "How do I configure multi-tenant isolation?"

## Recommendation for Queue Sharding Discussion

For the queue sharding HLD/RFC discussion:

**Primary recommendation**: Create **🏗️ Architecture & RFCs** category
- If creating new categories is possible, this provides clarity
- Separates major architectural work from smaller feature requests
- Makes it easy to find historical design decisions

**Fallback recommendation**: Use **💡 Ideas** category
- If limited to standard categories, Ideas is most appropriate
- Use [RFC] or [HLD] prefix in title: "**[RFC] Queue Sharding for Horizontal Scaling**"
- Tag with "architecture" or "design" labels if available

**Process**:
1. Open discussion with problem statement and high-level options
2. Gather community feedback over 1-2 weeks
3. Create formal HLD document in `docs/` (like `docs/24-queue-sharding-hld.md`)
4. Link discussion to document for continued review
5. After consensus, create implementation issues referencing both discussion and HLD

## Why Not Other Categories?

**Not General**: Too specific and technical for general conversation

**Not Q&A**: This is a proposal, not a question needing an answer

**Not Announcements**: Still in proposal phase, not decided yet

**Not Polls**: Need detailed discussion before voting on options

## Best Practices for Architecture Discussions

1. **Start with problem**: Explain current limitations (memory, CPU, I/O bottlenecks)
2. **Present options**: Compare alternatives (Option 1: vertical scaling, Option 2: master-replica, Option 3: RAFT)
3. **Show trade-offs**: Be honest about advantages and disadvantages
4. **Include migration**: How do existing users upgrade?
5. **Phase implementation**: Break into incremental milestones
6. **Link to code**: Show proposed interfaces, key formats, config examples
7. **Seek consensus**: Give community time to review and provide feedback

## Example: Queue Sharding Discussion Flow

1. **Discussion opened** in Ideas/Architecture category:
   - Title: "[RFC] Queue Sharding for Horizontal Scaling"
   - Content: Problem statement, three options overview, open questions
   
2. **Community feedback** (1-2 weeks):
   - Questions about RAFT maturity
   - Concerns about migration complexity
   - Alternative suggestions (Redis Cluster vs client-side routing)
   
3. **Formal HLD created**: `docs/24-queue-sharding-hld.md`
   - 888 lines of detailed design
   - Addresses all feedback from discussion
   - Includes code examples, migration strategy, phased roadmap
   
4. **Discussion updated** with link to HLD for final review

5. **Implementation issues** created after consensus:
   - Issue #31: Design and implement queue sharding (umbrella issue)
   - Sub-issues for Phase 1 (ShardSupplier), Phase 2 (multi-shard), etc.

## Conclusion

**For codeQ's architectural discussions like queue sharding HLD/RFC**:

✅ **Best**: Create dedicated **🏗️ Architecture & RFCs** category

✅ **Fallback**: Use **💡 Ideas** with [RFC] prefix

❌ **Avoid**: General (too broad), Q&A (not a question), Announcements (not yet decided)

This provides clear signal that major system changes follow a formal RFC process with community input before implementation.
