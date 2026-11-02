# golab2026
mysql interpolateParams usage example

Runs the same parameterized query with `go-sql-driver/mysql` twice, first with
`interpolateParams=false` (the default) and then with `interpolateParams=true`.
For each mode it prints what the MySQL server received, its session counters,
and how long the queries took.

| interpolateParams | What the driver sends per `db.Query(sql, args...)` | Round trips |
|---|---|---|
| `false` | `COM_STMT_PREPARE`, `COM_STMT_EXECUTE`, `COM_STMT_CLOSE` (server-side prepared statement) | 2 |
| `true`  | One `COM_QUERY`, with the args escaped into the SQL by the driver | 1 |

## Run

```sh
docker compose up -d --wait
go run .                        # local, ~0 latency
go run . -n 300 -latency 1ms    # simulate 1ms one-way latency (2ms RTT)
docker compose down
```

Flags: `-dsn` (or the `MYSQL_DSN` env var, default `root:root@tcp(127.0.0.1:3306)/demo`),
`-n` (queries per mode), `-latency` (one-way delay added by a built-in TCP proxy).

MySQL runs with `--general-log=1 --log-output=TABLE`, so the program reads
`mysql.general_log` to show `Prepare / Execute / Close stmt` versus a single `Query`.

## Example output (1ms latency)

```
interpolateParams=false  300 queries in 2.011s   (6.704ms/query)
interpolateParams=true   300 queries in 986ms    (3.286ms/query)
speedup with interpolateParams=true: 2.04x
```

## Trade-offs of `interpolateParams=true`

- One round trip instead of two, and no prepared statements to manage on the server.
- Escaping happens on the client (and respects `NO_BACKSLASH_ESCAPES`). The
  DSN is rejected if it combines `interpolateParams=true` with an unsafe
  multibyte collation (`big5`, `gbk`, `sjis`, `cp932`, ...).
- For some arg types or queries the driver cannot interpolate. Then it falls
  back to a prepared statement on its own.
- Explicit `db.Prepare` still uses server-side prepared statements, so reusing
  one prepared statement in a hot loop is a separate option.
