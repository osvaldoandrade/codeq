# GitHub Discussions Guide for codeQ

This guide helps you choose the right category for your discussion in the codeQ project.

## Available Categories

### 📣 Announcements
**Purpose**: Official updates from maintainers

**Use for**:
- Release announcements and changelogs
- Breaking changes and migration notices
- Project roadmap updates
- Security advisories
- Significant architectural decisions (after approval)

**Example topics**:
- "codeQ v1.2.0 Released with Rate Limiting Support"
- "Breaking Change: DLQ Changed from LIST to SET in v1.1.0"
- "Security: CVE-2026-XXXXX Fixed in Latest Release"

**Who can post**: Maintainers only

---

### 💬 General
**Purpose**: Open-ended conversations about the project

**Use for**:
- Broader discussions about codeQ's direction
- Community meetups or events
- Success stories and use cases
- Best practices and patterns
- Non-technical discussions

**Example topics**:
- "How are you using codeQ in production?"
- "Community call: Let's discuss multi-region deployments"
- "Show and tell: Our codeQ monitoring setup"

**Who can post**: Anyone

---

### 💡 Ideas
**Purpose**: Feature proposals and enhancement suggestions

**Use for**:
- New feature proposals
- API improvements
- Performance enhancement ideas
- Integration suggestions
- Minor improvements that don't need a full RFC

**Example topics**:
- "Add support for task dependencies"
- "Batch claim API for high-throughput workers"
- "PostgreSQL storage backend as alternative to KVRocks"

**Who can post**: Anyone

**Note**: Major architectural changes (like queue sharding) should start here, then move to a formal RFC/HLD document for detailed design.

---

### 🏗️ Architecture & RFCs (Recommended New Category)
**Purpose**: Technical design discussions for significant changes

**Use for**:
- High-level design (HLD) documents
- Request for Comments (RFC) on architecture
- Major system changes requiring community input
- Performance benchmarking results
- Breaking change proposals

**Example topics**:
- "RFC: Queue Sharding for Horizontal Scaling"
- "HLD: RAFT Consensus Integration with KVRocks"
- "Architecture Discussion: Multi-Region Active-Active"
- "Performance: Benchmarking Claim Latency Under Load"

**Who can post**: Anyone

**Process**:
1. Start discussion with problem statement
2. Gather community feedback
3. Author creates formal HLD/RFC document (in `docs/`)
4. Link discussion to the document for continued feedback
5. After consensus, create implementation issues

**Related**: See `docs/24-queue-sharding-hld.md` as an example RFC

---

### 🗳️ Polls
**Purpose**: Take votes from the community

**Use for**:
- Feature prioritization
- Naming decisions
- Configuration defaults
- Release schedule preferences

**Example topics**:
- "Which feature should we prioritize for v1.3?"
- "Preferred default lease timeout: 120s or 300s?"
- "Release cadence: monthly or quarterly?"

**Who can post**: Maintainers typically, but community members can suggest polls

---

### 🙏 Q&A
**Purpose**: Get help from the community

**Use for**:
- "How do I..." questions
- Troubleshooting deployment issues
- Configuration questions
- API usage questions
- Integration help

**Example topics**:
- "How do I configure multi-tenant isolation?"
- "Why are my tasks stuck in delayed queue?"
- "Best way to handle idempotency for webhooks?"
- "Error when claiming tasks: connection refused"

**Who can post**: Anyone

**Note**: Mark helpful answers as the solution to help future users.

---

## Choosing the Right Category

### Decision Tree

**Is it an official announcement?**
→ Yes: Use **📣 Announcements** (maintainers only)
→ No: Continue...

**Do you need help or have a question?**
→ Yes: Use **🙏 Q&A**
→ No: Continue...

**Is it a major architectural proposal?**
→ Yes: Use **🏗️ Architecture & RFCs** (or **💡 Ideas** if this category doesn't exist)
→ No: Continue...

**Is it a new feature idea?**
→ Yes: Use **💡 Ideas**
→ No: Continue...

**Do you want to take a vote?**
→ Yes: Use **🗳️ Polls**
→ No: Use **💬 General**

---

## Special Case: Architecture & Design Discussions

For significant architectural changes (like the queue sharding proposal), follow this workflow:

1. **Initial Discussion**: Start in **💡 Ideas** or **🏗️ Architecture & RFCs** with:
   - Problem statement
   - High-level approach
   - Open questions

2. **Gather Feedback**: Discuss trade-offs, alternatives, and concerns

3. **Formal RFC/HLD**: Author creates a detailed design document in `docs/`:
   - Current architecture and limitations
   - Requirements and goals
   - Proposed design with code examples
   - Trade-offs and alternatives
   - Migration strategy
   - Implementation roadmap

4. **Review Discussion**: Link the discussion to the RFC document for continued feedback

5. **Implementation**: After consensus, create issues for implementation phases

**Example**: The queue sharding discussion led to `docs/24-queue-sharding-hld.md`, which evaluates three architectural options and provides a phased implementation roadmap.

---

## Tips for Great Discussions

### For All Categories

- **Use descriptive titles**: "RFC: Queue Sharding" not "Thoughts?"
- **Provide context**: Link to related issues, PRs, or documentation
- **Be specific**: Include error messages, config snippets, or code examples
- **Stay on topic**: Start a new discussion if the conversation diverges
- **Be respectful**: Follow the Code of Conduct

### For Architecture Discussions

- **Start with the problem**: Explain why current approach doesn't work
- **Consider alternatives**: Present multiple options with trade-offs
- **Include code examples**: Show proposed interfaces or key format changes
- **Address migration**: How do existing deployments upgrade?
- **Think in phases**: Break large changes into incremental steps

### For Q&A

- **Search first**: Check existing discussions and documentation
- **Include versions**: Go version, codeQ version, KVRocks version
- **Provide logs**: Include relevant error messages (redact sensitive data)
- **Show what you tried**: Explain troubleshooting steps already taken
- **Mark solutions**: Help future users by marking the accepted answer

---

## Migration from Issues to Discussions

If you opened an issue that's better suited as a discussion:

- **Feature requests** → 💡 Ideas
- **Questions** → 🙏 Q&A
- **General feedback** → 💬 General
- **Architecture proposals** → 🏗️ Architecture & RFCs

Maintainers may convert issues to discussions to keep the issue tracker focused on bugs and accepted work items.

---

## Related Resources

- [Contributing Guide](../CONTRIBUTING.md)
- [Documentation Index](../docs/README.md)
- [Issue Templates](../ISSUE_TEMPLATE/)
- [Queue Sharding HLD Example](../docs/24-queue-sharding-hld.md)
