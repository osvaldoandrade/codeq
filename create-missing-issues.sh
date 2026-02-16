#!/bin/bash
#
# Script to create missing issues for the Daily Status Report execution plan
# These issues are recommended in the Daily Status Report - February 16, 2026 (Issue #158)
#
# Usage: ./create-missing-issues.sh [--dry-run]
#
# Options:
#   --dry-run   Show what would be created without actually creating issues
#

set -e

DRY_RUN=false
if [ "$1" = "--dry-run" ]; then
    DRY_RUN=true
    echo "🔍 DRY RUN MODE - No issues will be created"
    echo ""
fi

REPO="osvaldoandrade/codeq"
ISSUE_BODIES_DIR=".github/issue-bodies"

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "Creating missing issues for Daily Status Report execution plan"
echo "Repository: $REPO"
echo ""

# Issue 1: Python SDK Specification
echo -e "${BLUE}Issue 1: Python SDK Specification${NC}"
TITLE="Python SDK Specification and Design Document"
LABELS="enhancement,documentation,sdk,python"
BODY_FILE="$ISSUE_BODIES_DIR/python-sdk-spec.md"

if [ "$DRY_RUN" = true ]; then
    echo "  Title: $TITLE"
    echo "  Labels: $LABELS"
    echo "  Body: $BODY_FILE"
else
    gh issue create \
        --repo "$REPO" \
        --title "$TITLE" \
        --label "$LABELS" \
        --body-file "$BODY_FILE"
    echo -e "${GREEN}✓ Created${NC}"
fi
echo ""

# Issue 2: JavaScript/TypeScript SDK Specification
echo -e "${BLUE}Issue 2: JavaScript/TypeScript SDK Specification${NC}"
TITLE="JavaScript/TypeScript SDK Specification and Design Document"
LABELS="enhancement,documentation,sdk,javascript,typescript"
BODY_FILE="$ISSUE_BODIES_DIR/js-sdk-spec.md"

if [ "$DRY_RUN" = true ]; then
    echo "  Title: $TITLE"
    echo "  Labels: $LABELS"
    echo "  Body: $BODY_FILE"
else
    gh issue create \
        --repo "$REPO" \
        --title "$TITLE" \
        --label "$LABELS" \
        --body-file "$BODY_FILE"
    echo -e "${GREEN}✓ Created${NC}"
fi
echo ""

# Issue 3: Load Testing Baseline Results
echo -e "${BLUE}Issue 3: Load Testing Baseline Results${NC}"
TITLE="Load Testing Baseline Results Collection and Tracking"
LABELS="performance,testing,help wanted,good first issue"
BODY_FILE="$ISSUE_BODIES_DIR/load-testing-baselines.md"

if [ "$DRY_RUN" = true ]; then
    echo "  Title: $TITLE"
    echo "  Labels: $LABELS"
    echo "  Body: $BODY_FILE"
else
    gh issue create \
        --repo "$REPO" \
        --title "$TITLE" \
        --label "$LABELS" \
        --body-file "$BODY_FILE"
    echo -e "${GREEN}✓ Created${NC}"
fi
echo ""

# Issue 4: Queue Sharding HLD Feedback
echo -e "${BLUE}Issue 4: Queue Sharding HLD Feedback${NC}"
TITLE="Queue Sharding HLD: Feedback Collection and Discussion"
LABELS="design,discussion,scalability,queue-sharding"
BODY_FILE="$ISSUE_BODIES_DIR/sharding-hld-feedback.md"

if [ "$DRY_RUN" = true ]; then
    echo "  Title: $TITLE"
    echo "  Labels: $LABELS"
    echo "  Body: $BODY_FILE"
else
    gh issue create \
        --repo "$REPO" \
        --title "$TITLE" \
        --label "$LABELS" \
        --body-file "$BODY_FILE"
    echo -e "${GREEN}✓ Created${NC}"
fi
echo ""

# Issue 5: Documentation Audit
echo -e "${BLUE}Issue 5: Documentation Audit${NC}"
TITLE="Documentation Audit and Continuous Improvement Tracking"
LABELS="documentation,help wanted,good first issue"
BODY_FILE="$ISSUE_BODIES_DIR/documentation-audit.md"

if [ "$DRY_RUN" = true ]; then
    echo "  Title: $TITLE"
    echo "  Labels: $LABELS"
    echo "  Body: $BODY_FILE"
else
    gh issue create \
        --repo "$REPO" \
        --title "$TITLE" \
        --label "$LABELS" \
        --body-file "$BODY_FILE"
    echo -e "${GREEN}✓ Created${NC}"
fi
echo ""

# Issue 6: Plugin Architecture Phase 1
echo -e "${BLUE}Issue 6: Plugin Architecture Phase 1${NC}"
TITLE="Plugin Architecture Implementation - Phase 1: Core Interfaces and Registry"
LABELS="enhancement,architecture,plugins,p2"
BODY_FILE="$ISSUE_BODIES_DIR/plugin-architecture-phase1.md"

if [ "$DRY_RUN" = true ]; then
    echo "  Title: $TITLE"
    echo "  Labels: $LABELS"
    echo "  Body: $BODY_FILE"
else
    gh issue create \
        --repo "$REPO" \
        --title "$TITLE" \
        --label "$LABELS" \
        --body-file "$BODY_FILE"
    echo -e "${GREEN}✓ Created${NC}"
fi
echo ""

if [ "$DRY_RUN" = true ]; then
    echo -e "${YELLOW}DRY RUN COMPLETE - No issues were created${NC}"
    echo "To create the issues, run: ./create-missing-issues.sh"
else
    echo -e "${GREEN}✓ All 6 issues created successfully!${NC}"
    echo ""
    echo "View all issues: https://github.com/$REPO/issues"
fi
