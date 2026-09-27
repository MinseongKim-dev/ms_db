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

- One Claude session per phase; at phase end the user writes a summary in `progress.md` and runs `/clear`.
- Guardrail hooks (planned): same-error circuit breaker (block after 2 consecutive identical `go build`/`go test` failures per file+error signature), race-detector output compressor, custom `go/ast`/`go/packages` dependency graph from Phase 3. All hooks must **fail open** (exit 0 on internal error).

## Working with the user (learning project)

The purpose of this repo is for the user to learn DB internals (DDIA + systems programming coursework), so these rules take priority over speed:

1. **Completion criterion first.** Every implementation request should open with a one-line completion criterion for the phase (e.g. "SlottedPage must not corrupt variable-length tuples at extreme sizes"). If a request lacks one, ask for it before implementing — do not invent your own criterion, since drifting criteria break consistency between modules.
2. **No commits of concurrency/recovery code before the user reviews it.** For MVCC (Phase 3), ARIES (Phase 4), and Raft (Phase 5), stop after tests pass and hand over for line-by-line review, even if tests are green. Point out the edge cases worth checking (e.g. "what happens if two transactions enter this lock at once?"), since bugs outside test coverage are not caught by passing tests.
3. **Be ready to justify design choices.** After delivering code, the user will ask why a data structure was chosen or what breaks without a given lock. Answer with the concrete failure scenario, not just the conclusion.
4. **Diagnose before fixing.** When a test fails, explain the root cause first and wait for the user before changing code. Do not jump straight to a fix.
5. **Do not write `progress.md` session summaries.** The user writes their own 3-line summary at the end of each session.
6. **When the circuit breaker trips, stop.** Do not retry or look for a workaround. Tell the user the approach is likely wrong and point to the relevant DDIA chapter or course material to re-read.
