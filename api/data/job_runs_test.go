package data

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alfg/openencoder/api/types"
	"github.com/jmoiron/sqlx"
)

// The tests in this file exercise the real SQL of the execution guards. They use
// sqlmock instead of Postgres so they run anywhere, but the statements, argument
// binding, row scanning and error mapping are the production code paths: if a
// guard clause or a run id condition is dropped from a query, the corresponding
// expectation below stops matching and the test fails.

// mockConnector exposes the sqlmock driver so every connect() call gets its own
// *sql.DB. The production code opens and closes a handle per operation; sharing
// one *sql.DB would make the second call fail with "database is closed", while a
// fresh pool over the same driver keeps the registered expectations intact.
type mockConnector struct {
	drv driver.Driver
	dsn string
}

func (c mockConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &keepOpenConn{Conn: conn}, nil
}

func (c mockConnector) Driver() driver.Driver { return c.drv }

// sanitizeDSN turns a test name into a DSN safe for sqlmock's registry.
func sanitizeDSN(name string) string {
	return strings.NewReplacer("/", "_", " ", "_", "\\", "_").Replace(name)
}

// keepOpenConn makes conn.Close a no-op. sqlmock closes itself when its last
// connection closes, but the production code closes its handle after every
// operation; keeping the mock connection open lets one expectation queue span
// several handles.
type keepOpenConn struct{ driver.Conn }

func (c *keepOpenConn) Close() error { return nil }

func (c *keepOpenConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

func (c *keepOpenConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}

func (c *keepOpenConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, driver.ErrSkip
	}
	return beginner.BeginTx(ctx, opts)
}

func (c *keepOpenConn) CheckNamedValue(nv *driver.NamedValue) error {
	checker, ok := c.Conn.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return checker.CheckNamedValue(nv)
}

// whitespaceMatcher compares SQL statements ignoring insignificant whitespace.
// The production queries are multi-line templates; the guard clauses are what
// matters, not their indentation.
var whitespaceMatcher = sqlmock.QueryMatcherFunc(func(expected, actual string) error {
	normal := func(s string) string {
		s = strings.NewReplacer("(", " ( ", ")", " ) ", ",", " , ").Replace(s)
		return strings.Join(strings.Fields(s), " ")
	}
	if normal(expected) != normal(actual) {
		return fmt.Errorf("actual sql: %q does not equal expected %q", normal(actual), normal(expected))
	}
	return nil
})

var mockDSNCounter uint64

func withMockDB(t *testing.T, setup func(sqlmock.Sqlmock)) {
	t.Helper()

	// The DSN must be unique per invocation because sqlmock keeps a global
	// registry and `go test -count=N` reruns tests in the same process.
	dsn := fmt.Sprintf("jobguard_%s_%d", sanitizeDSN(t.Name()), atomic.AddUint64(&mockDSNCounter, 1))
	db, mock, err := sqlmock.NewWithDSN(dsn,
		sqlmock.QueryMatcherOption(whitespaceMatcher))
	if err != nil {
		t.Fatal(err)
	}
	conn := mockConnector{drv: db.Driver(), dsn: dsn}

	old := connect
	connect = func() (*sqlx.DB, error) {
		return sqlx.NewDb(sql.OpenDB(conn), "sqlmock"), nil
	}
	t.Cleanup(func() {
		connect = old
		db.Close()
	})
	setup(mock)
}

const (
	claimQuery          = `UPDATE jobs SET run_id = $2, cancel_requested = false WHERE guid = $1 AND run_id IS NULL AND NOT cancel_requested AND status NOT IN ('cancelled', 'error', 'completed') RETURNING guid, status, run_id, cancel_requested`
	resetProgressQuery  = `UPDATE encode SET progress = 0, speed = NULL, fps = 0 WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2)`
	stateQuery          = `SELECT guid, status, run_id, cancel_requested FROM jobs WHERE guid = $1`
	updateStatusQuery   = `UPDATE jobs SET status = $3 WHERE guid = $1 AND run_id = $2 AND status NOT IN ('cancelled', 'error', 'completed')`
	encodeProgressQuery = `UPDATE encode SET progress = $3, speed = $4, fps = $5 WHERE job_id = (SELECT id FROM jobs WHERE guid = $1 AND run_id = $2) AND EXISTS (SELECT 1 FROM jobs WHERE guid = $1 AND run_id = $2 AND status NOT IN ('cancelled', 'error', 'completed'))`
	finishQuery         = `UPDATE jobs SET status = CASE WHEN status = 'cancelled' OR cancel_requested THEN 'cancelled' ELSE $3 END, run_id = NULL, cancel_requested = false WHERE guid = $1 AND run_id = $2 AND status NOT IN ('cancelled', 'error', 'completed') RETURNING status`
	cancelByGUIDQuery   = `UPDATE jobs SET status = 'cancelled', cancel_requested = true WHERE guid = $1 AND status NOT IN ('completed', 'error', 'cancelled')`
	cancelByIDQuery     = `UPDATE jobs SET status = 'cancelled', cancel_requested = true WHERE id = $1 AND status NOT IN ('completed', 'error', 'cancelled')`
	restartQuery        = `UPDATE jobs SET status = 'restarting', run_id = NULL, cancel_requested = false WHERE guid = $1`
	restartProgress     = `UPDATE encode SET progress = 0, speed = NULL, fps = 0 WHERE job_id = (SELECT id FROM jobs WHERE guid = $1)`
)

func TestClaimJobRunOnlyWhenUnownedAndActive(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(claimQuery).
			WithArgs("job-guid", "run-1").
			WillReturnRows(sqlmock.NewRows([]string{"guid", "status", "run_id", "cancel_requested"}).
				AddRow("job-guid", types.JobQueued, "run-1", false))
		// A newly claimed run must start from clean progress.
		mock.ExpectExec(resetProgressQuery).
			WithArgs("job-guid", "run-1").
			WillReturnResult(sqlmock.NewResult(0, 1))
	})

	state, claimed, err := JobsOp{}.ClaimJobRun("job-guid", "run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !claimed {
		t.Fatal("expected the run to be claimed")
	}
	if state.RunID.String != "run-1" || state.Status != types.JobQueued {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestClaimJobRunRejectedWhenAlreadyOwned(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(claimQuery).
			WithArgs("job-guid", "run-2").
			WillReturnRows(sqlmock.NewRows([]string{"guid", "status", "run_id", "cancel_requested"}))
		mock.ExpectQuery(stateQuery).
			WithArgs("job-guid").
			WillReturnRows(sqlmock.NewRows([]string{"guid", "status", "run_id", "cancel_requested"}).
				AddRow("job-guid", types.JobEncoding, "run-1", false))
	})

	state, claimed, err := JobsOp{}.ClaimJobRun("job-guid", "run-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claimed {
		t.Fatal("a job owned by another run must not be claimed twice")
	}
	if state.RunID.String != "run-1" || state.Status != types.JobEncoding {
		t.Fatalf("unexpected owner: %+v", state)
	}
}

func TestUpdateJobRunStatusRejectsStaleRun(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(updateStatusQuery).
			WithArgs("job-guid", "old-run", types.JobUploading).
			WillReturnResult(sqlmock.NewResult(0, 0))
	})

	if err := (JobsOp{}).UpdateJobRunStatus("job-guid", "old-run", types.JobUploading); err != types.ErrStaleRun {
		t.Fatalf("error = %v, want ErrStaleRun", err)
	}
}

func TestUpdateJobRunStatusAcceptsOwningRun(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(updateStatusQuery).
			WithArgs("job-guid", "run-1", types.JobEncoding).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})

	if err := (JobsOp{}).UpdateJobRunStatus("job-guid", "run-1", types.JobEncoding); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateEncodeProgressForRunRejectsStaleRun(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(encodeProgressQuery).
			WithArgs("job-guid", "old-run", 99.0, "9x", 999.0).
			WillReturnResult(sqlmock.NewResult(0, 0))
	})

	err := (JobsOp{}).UpdateEncodeProgressForRun("job-guid", "old-run", 99, "9x", 999)
	if err != types.ErrStaleRun {
		t.Fatalf("error = %v, want ErrStaleRun", err)
	}
}

func TestFinishJobRunPrefersCancelledAndReleasesRun(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(finishQuery).
			WithArgs("job-guid", "run-1", types.JobCompleted).
			WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(types.JobCancelled))
	})

	final, err := JobsOp{}.FinishJobRun("job-guid", "run-1", types.JobCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if final != types.JobCancelled {
		t.Fatalf("final status = %q, want cancelled (cancel wins over completion)", final)
	}
}

func TestFinishJobRunReportsPersistedStatusWhenStale(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		// The finish UPDATE matches no rows: the job is already terminal or owned
		// by another execution.
		mock.ExpectQuery(finishQuery).
			WithArgs("job-guid", "run-1", types.JobCompleted).
			WillReturnRows(sqlmock.NewRows([]string{"status"}))
		mock.ExpectQuery(stateQuery).
			WithArgs("job-guid").
			WillReturnRows(sqlmock.NewRows([]string{"guid", "status", "run_id", "cancel_requested"}).
				AddRow("job-guid", types.JobRestarting, "run-2", false))
	})

	final, err := JobsOp{}.FinishJobRun("job-guid", "run-1", types.JobCompleted)
	if err != types.ErrStaleRun {
		t.Fatalf("error = %v, want ErrStaleRun", err)
	}
	if final != types.JobRestarting {
		t.Fatalf("final status = %q, want the persisted restarting status", final)
	}
}

func TestRequestJobCancelByGUIDReportsNoOpForTerminalJob(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(cancelByGUIDQuery).
			WithArgs("job-guid").
			WillReturnResult(sqlmock.NewResult(0, 0))
	})

	ok, err := JobsOp{}.RequestJobCancelByGUID("job-guid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("a terminal job must not be cancellable")
	}
}

func TestRequestJobCancelByIDMarksJob(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(cancelByIDQuery).
			WithArgs(7).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})

	ok, err := JobsOp{}.RequestJobCancelByID(7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected the cancel to apply")
	}
}

func TestRestartJobRunClearsOwnershipAndResetsProgress(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(restartQuery).
			WithArgs("job-guid").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(restartProgress).
			WithArgs("job-guid").
			WillReturnResult(sqlmock.NewResult(0, 1))
	})

	ok, err := JobsOp{}.RestartJobRunByGUID("job-guid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected the restart to apply")
	}
}
