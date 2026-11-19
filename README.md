# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/roberttijani/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/roberttijani/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A single-process GitHub-compatible git forge in TypeScript for Node.js 22+. Built by an AI agent for make.fun's "Build a GitHub-compatible git forge" bounty.

## Overview

This implementation provides a standalone, dependency-free in-memory Git forge supporting all 4 contest surfaces:
- **Git Smart HTTP**: Full Git protocol v2 and v0 smart transfer over HTTP (`info/refs`, `git-upload-pack`, `git-receive-pack`), shallow fetches (`depth`, `deepen`, `shallow`, `unshallow`), sideband-64k framing, and packfile generation.
- **GitHub REST API v3**: `/api/v3` endpoints covering repositories, branches, commits, contents, issues, comments, labels, and pull requests with 3-way merge conflict detection and squash merging.
- **GitHub GraphQL API v4**: `/api/graphql` supporting queries (`viewer`, `repository`, `node`, `issues`, `pullRequests`, introspection) and mutations (`createIssue`, `closeIssue`, `createPullRequest`, `mergePullRequest`, `addComment`).
- **Model Context Protocol (MCP)**: `/mcp` streamable HTTP transport implementing MCP protocol versions 2026-07-28 and 2025-11-25 with core GitHub tools (`create_repo`, `get_repo`, `create_issue`, `list_issues`, `create_pull_request`, `merge_pull_request`).

## Run

```sh
node --experimental-strip-types main.ts
```

The server starts without dependencies on Node 22+ using native TypeScript type-stripping.

## License

[MIT](LICENSE)
