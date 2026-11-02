// Command golab2026 shows what changes in go-sql-driver/mysql when the DSN
// option interpolateParams is false (default) versus true.
//
//	interpolateParams=false: a query with args becomes a server-side prepared
//	statement: COM_STMT_PREPARE + COM_STMT_EXECUTE + COM_STMT_CLOSE.
//	interpolateParams=true:  the driver escapes the args into the SQL text and
//	sends one COM_QUERY.
package main

import (
	"cmp"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

func main() {
	dsn := flag.String("dsn", cmp.Or(os.Getenv("MYSQL_DSN"), "root:root@tcp(127.0.0.1:3306)/demo"), "base MySQL DSN")
	n := flag.Int("n", 2000, "number of queries to run per mode")
	latency := flag.Duration("latency", 0, "simulated one-way network latency (e.g. 1ms) added by a local TCP proxy")
	flag.Parse()

	ctx := context.Background()

	cfg, err := mysql.ParseDSN(*dsn)
	if err != nil {
		log.Fatalf("parse dsn: %v", err)
	}

	admin, err := openDB(cfg, false)
	if err != nil {
		log.Fatal(err)
	}
	defer admin.Close()
	if err := setup(ctx, admin); err != nil {
		log.Fatalf("setup: %v", err)
	}

	if *latency > 0 {
		proxyAddr, err := startLatencyProxy(cfg.Addr, *latency)
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
		cfg.Addr = proxyAddr
		fmt.Printf("Simulating %v one-way latency (%v per round trip) via proxy %s\n", *latency, 2**latency, proxyAddr)
	}

	results := make(map[bool]time.Duration)
	for _, interpolate := range []bool{false, true} {
		elapsed, err := runMode(ctx, admin, cfg, interpolate, *n)
		if err != nil {
			log.Fatalf("interpolateParams=%v: %v", interpolate, err)
		}
		results[interpolate] = elapsed
	}

	fmt.Println("\n=== Summary ===")
	for _, interpolate := range []bool{false, true} {
		d := results[interpolate]
		fmt.Printf("interpolateParams=%-5v  %d queries in %-12v  (%v/query)\n",
			interpolate, *n, d.Round(time.Millisecond), (d / time.Duration(*n)).Round(time.Microsecond))
	}
	fmt.Printf("speedup with interpolateParams=true: %.2fx\n", float64(results[false])/float64(results[true]))
}

func openDB(base *mysql.Config, interpolate bool) (*sql.DB, error) {
	cfg := base.Clone()
	cfg.InterpolateParams = interpolate
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

func setup(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		"DROP TABLE IF EXISTS items",
		`CREATE TABLE items (
			id INT PRIMARY KEY,
			name VARCHAR(64) NOT NULL,
			price DECIMAL(10,2) NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`INSERT INTO items VALUES
			(1, 'gopher plush', 19.99, '2026-01-01 10:00:00'),
			(2, 'go mug',        9.50, '2026-02-02 11:00:00'),
			(3, "o'reilly book", 49.00, '2026-03-03 12:00:00')`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func runMode(ctx context.Context, admin *sql.DB, cfg *mysql.Config, interpolate bool, n int) (time.Duration, error) {
	fmt.Printf("\n=== interpolateParams=%v ===\n", interpolate)

	db, err := openDB(cfg, interpolate)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	// Pin one connection so session counters and the general log cover
	// exactly the queries issued below.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	var connID int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID); err != nil {
		return 0, err
	}

	before, err := sessionStatus(ctx, conn)
	if err != nil {
		return 0, err
	}

	// Many queries with args: count what the server received and measure time.
	names := []string{"gopher plush", "go mug", "o'reilly book"}
	start := time.Now()
	for i := range n {
		var id int
		if err := conn.QueryRowContext(ctx, "SELECT id FROM items WHERE name = ?", names[i%len(names)]).Scan(&id); err != nil {
			return 0, err
		}
	}
	elapsed := time.Since(start)

	after, err := sessionStatus(ctx, conn)
	if err != nil {
		return 0, err
	}
	if err := printGeneralLog(ctx, admin, connID); err != nil {
		return 0, err
	}
	fmt.Printf("Server session counters for %d queries:\n", n)
	for _, name := range statusVars {
		fmt.Printf("  %-17s %d\n", name, after[name]-before[name])
	}
	fmt.Printf("Elapsed: %v\n", elapsed.Round(time.Millisecond))
	return elapsed, nil
}

var statusVars = []string{"Com_stmt_prepare", "Com_stmt_execute", "Com_stmt_close", "Com_select"}

func sessionStatus(ctx context.Context, conn *sql.Conn) (map[string]int64, error) {
	// No args, so this is a plain COM_QUERY in both modes.
	rows, err := conn.QueryContext(ctx, "SHOW SESSION STATUS WHERE Variable_name IN ('"+strings.Join(statusVars, "','")+"')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var name string
		var value int64
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}

func printGeneralLog(ctx context.Context, admin *sql.DB, connID int64) error {
	rows, err := admin.QueryContext(ctx,
		`SELECT command_type, CONVERT(argument USING utf8mb4)
		   FROM mysql.general_log
		  WHERE thread_id = ? AND command_type <> 'Connect'
		    AND argument NOT LIKE 'SELECT CONNECTION_ID()%'
		    AND argument NOT LIKE 'SHOW SESSION STATUS%'
		  ORDER BY event_time
		  LIMIT 7`,
		connID)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("First entries in the server general log for connection %d:\n", connID)
	for rows.Next() {
		var cmd string
		var arg string
		if err := rows.Scan(&cmd, &arg); err != nil {
			return err
		}
		fmt.Printf("  %-8s %s\n", cmd, arg)
	}
	return rows.Err()
}

// startLatencyProxy listens on a random local port and forwards to target,
// delaying every chunk by d in each direction. Delays are pipelined (each
// chunk is due at arrival+d), so it behaves like real link latency.
func startLatencyProxy(target string, d time.Duration) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				continue
			}
			go delayedCopy(server, client, d)
			go delayedCopy(client, server, d)
		}
	}()
	return ln.Addr().String(), nil
}

func delayedCopy(dst net.Conn, src net.Conn, d time.Duration) {
	type chunk struct {
		due  time.Time
		data []byte
	}
	ch := make(chan chunk, 1024)
	go func() {
		defer close(ch)
		buf := make([]byte, 64*1024)
		for {
			k, err := src.Read(buf)
			if k > 0 {
				ch <- chunk{due: time.Now().Add(d), data: append([]byte(nil), buf[:k]...)}
			}
			if err != nil {
				return
			}
		}
	}()
	defer dst.Close()
	for c := range ch {
		time.Sleep(time.Until(c.due))
		if _, err := dst.Write(c.data); err != nil {
			io.Copy(io.Discard, src)
			return
		}
	}
}
