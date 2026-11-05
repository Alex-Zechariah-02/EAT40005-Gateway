package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Seeding writes test state directly with the production schema. It stands in
// for the installation, enrolment and publication steps that dedicated tests
// exercise through the real commands and Admin API.

// The harness administrator holds every permission. Its password exists only
// in this test process.
const harnessAdmin = "harness"

var (
	harnessPassword = "harness-" + newToken()
	harnessHash     = sync.OnceValue(func() string { return hashPassword(harnessPassword) })
)

type seedOptions struct {
	mode        string
	highWater   time.Time
	provisional *policy.Policy
	deadline    time.Time
	version     int // database schema version; default the current one
}

// seed creates a disposable database whose confirmed revision 1 is policy,
// with the harness administrator enrolled.
func seed(t *testing.T, path string, policy policy.Policy, opts seedOptions) {
	t.Helper()
	if opts.mode == "" {
		opts.mode = store.ModeNormal
	}
	if opts.highWater.IsZero() {
		opts.highWater = time.Now()
	}
	var episode any
	if opts.mode == store.ModeRecovery {
		episode = randomHex(16)
	}
	db, err := sql.Open("sqlite", store.DatabaseURI(path)+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if opts.version == 0 {
		opts.version = store.DatabaseVersion
	}
	if err := store.ApplyMigrations(ctx, tx, 0, opts.version); err != nil {
		t.Fatal(err)
	}

	type statement struct {
		query string
		args  []any
	}
	statements := []statement{
		{`INSERT INTO deployment (id, installation, mode, recovery_episode, high_water) VALUES (1, ?, ?, ?, ?)`,
			[]any{randomHex(16), opts.mode, episode, opts.highWater.UnixMilli()}},
		{`INSERT INTO revision VALUES (1, NULL, 'genesis', ?, ?, 'seed', ?)`, revisionArgs(policy)},
		{`INSERT INTO base VALUES (1, 1, NULL, NULL)`, nil},
		{`INSERT INTO administrator VALUES (?, ?, ?, 'active', 1, ?)`, []any{harnessAdmin, store.PermAll, harnessHash(), time.Now().UnixMilli()}},
	}
	// A provisional revision 2 comes from candidate 1 and has the activation
	// record the schema requires, with revision 1 as its rollback target.
	// Schema version 1 had no activation record.
	if opts.provisional != nil {
		deadline := opts.deadline.UnixMilli()
		statements = append(statements,
			statement{`INSERT INTO candidate VALUES (1, 1, ?, ?, 0, 'established', 'seed', ?)`, revisionArgs(*opts.provisional)},
			statement{`INSERT INTO revision VALUES (2, 1, 'publish', ?, ?, 'seed', ?)`, revisionArgs(*opts.provisional)},
		)
		if opts.version > 1 {
			statements = append(statements, statement{
				`INSERT INTO activation (revision, candidate, rollback_target, deadline, activated_at, actor) VALUES (2, 1, 1, ?, ?, 'seed')`,
				[]any{deadline, deadline - time.Minute.Milliseconds()}})
		}
		statements = append(statements, statement{`UPDATE base SET provisional = 2, deadline = ? WHERE id = 1`, []any{deadline}})
	}
	for _, s := range statements {
		if _, err := tx.ExecContext(ctx, s.query, s.args...); err != nil {
			t.Fatalf("seeding database: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func revisionArgs(p policy.Policy) []any {
	document, _ := json.Marshal(p)
	return []any{document, store.DocumentChecksum(document), time.Now().UnixMilli()}
}

// inspect opens a stopped gateway's database for assertions.
func inspect(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", store.DatabaseURI(path)+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// queryInt runs one query against a stopped gateway's database and closes it
// again, so the next gateway start can take its exclusive lock.
func queryInt(t *testing.T, path, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", store.DatabaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return count(t, db, query, args...)
}

// queryString is queryInt for a text result.
func queryString(t *testing.T, path, query string, args ...any) string {
	t.Helper()
	db, err := sql.Open("sqlite", store.DatabaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s sql.NullString
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s.String
}

// queryRows runs one query against a stopped gateway's database and returns
// every result row in Go syntax, so rows compare exactly, NULL included.
func queryRows(t *testing.T, path, query string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", store.DatabaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var list []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		list = append(list, fmt.Sprintf("%#v", values))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return list
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}
