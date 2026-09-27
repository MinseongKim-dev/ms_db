# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

MS_DB is a personal toy database engine written from scratch in Go (module `github.com/MinseongKim-dev/ms_db`, Go 1.22), following a 5-phase roadmap equivalent to CMU 15-445 BusTub. The repo is currently a skeleton: `storage/`, `main.go` contain only package declarations; `execution/` and `transaction/` hold only READMEs. The design below comes from the project PRD/TRD and describes the target, not existing code.

## Commands

```bash
go build ./...                          # build everything
go vet ./...                            # lint
go test ./...                           # all tests
go test ./storage -run TestName -v      # single test
go test -race ./...                     # required for txn/recovery/consensus work
```

`prototype/` holds Python sketches (e.g. `ra_iterator.py` for the relational-algebra iterator) used to explore ideas before writing Go; they are not part of the build.

## Architecture (target)

Single Go binary with layered modules. Dependency direction is strictly **storage → txn/query → recovery → consensus**; never add a reverse import.

| Module | Current dir | Responsibility |
| --- | --- | --- |
| storage | `storage/` | DiskManager (4KB page I/O, per-page CRC32 checksum to detect torn pages), SlottedPage for variable-length tuples, B-Tree index, vector-embedding byte layout (ANN/HNSW deferred to a later phase) |
| query | `execution/` | SQL parser → AST → relational-algebra plan nodes → Volcano-style iterators (Scan/Join/Project); Block Nested Loop Join for large joins. Optional Groq "Agentic Optimizer" with per-query-shape caching and a heuristic fallback (join smaller table first) on API failure/timeout |
| txn | `transaction/` | `sync.RWMutex`-based 2PL first, then **fully replaced** by MVCC (append-only tuple versions, `AS OF` snapshot queries). Never keep 2PL and MVCC side by side |
| recovery | — | WALManager (log written before page modification), ARIES recovery (Analysis → Redo → Undo) |
| consensus | — | Raft leader election + log replication across ≥3 processes (membership change / log compaction deferred); net/rpc or gRPC |
| dashboard | — | Three.js + WebSocket visualization of engine events |

## Design rules

- **AI is observer-only in correctness-critical paths.** ARIES and Raft must be 100% deterministic code. The Ollama agent may only observe the WAL / deadlock queue and report anomalies; rollback decisions and execution belong to the deterministic engine.
- The engine→dashboard event bus uses buffered channels with **non-blocking send**, so a slow or broken dashboard can never back-pressure the engine.
- Complete each phase's required implementation (tests passing) before starting its "Advanced" stretch goal.

## Testing strategy

- Phases 1–2: write BusTub-style phase test specs **before** the implementation.
- Phase 4: crash/fault-injection tests for ARIES recovery.
- Phase 5: MIT 6.824-style harness that injects network partitions; Raft correctness is not considered verified without it.
- Playwright is limited to dashboard smoke tests (render, WebSocket connection, per-event color change); never use it to verify engine concurrency/failure behavior.

## Workflow conventions

- One Claude session per phase; at phase end, record a summary in `progress.md` and `/clear`.
- Guardrail hooks (planned): same-error circuit breaker (block after 2 consecutive identical `go build`/`go test` failures per file+error signature), race-detector output compressor, custom `go/ast`/`go/packages` dependency graph from Phase 3. All hooks must **fail open** (exit 0 on internal error).
