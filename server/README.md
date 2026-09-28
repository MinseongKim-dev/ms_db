# server module (stretch: PostgreSQL wire protocol)

Goal: a Go TCP server on port 5432 that speaks enough of the PostgreSQL v3 wire
protocol for the standard pgjdbc driver to connect, run PreparedStatements and
read ResultSets. Built first against a stub executor, then wired to
`execution/` after Phase 2.

## Scope decisions

| Area | Decision |
| --- | --- |
| Auth | `trust` only: reply `AuthenticationOk` immediately. No SCRAM/MD5. |
| Query protocol | Extended Query (Parse → Bind → Describe → Execute → Sync) is required from the start. pgjdbc uses it by default (`preferQueryMode=extended`) even for a plain `Statement` with no parameters. Simple Query (`Q`) is also supported; `?preferQueryMode=simple` in the JDBC URL can be used to test it alone early on. |
| Result format | Text only. Read the result-format codes in `Bind`; if binary is requested, return an error rather than silently sending text. pgjdbc switches to binary for some types after `prepareThreshold` (default 5) executions of the same statement; clients can set `binaryTransfer=false`. |
| Types (OID) | Start with `bool`=16, `int4`=23, `text`=25. Likely next: `int8`=20, `varchar`=1043. `RowDescription` must carry the correct OID per column. |
| Clients | Plain JDBC / JdbcTemplate. JPA/Hibernate is out of scope (needs `pg_catalog` emulation). |

## Handshake checklist (pgjdbc)

1. Driver may send `SSLRequest` first (default `sslmode=prefer`) → reply the single byte `'N'`.
2. `StartupMessage` → `AuthenticationOk`.
3. `ParameterStatus` for at least `server_version`, `client_encoding=UTF8`, `standard_conforming_strings=on`, `DateStyle=ISO, MDY`, `integer_datetimes=on`. The driver rejects a non-UTF8 `client_encoding`.
4. `BackendKeyData`, then `ReadyForQuery`.
5. Driver sends setup statements such as `SET extra_float_digits = 3`; the stub answers `SET` with `CommandComplete` only.

These notes are from memory of pgjdbc behaviour; verify each against a real
pgjdbc connection when implementing.
