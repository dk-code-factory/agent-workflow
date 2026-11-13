package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	_ "modernc.org/sqlite"
)

type Ledger struct {
	db   *sql.DB
	Root string
}

const ledgerSchema = `
CREATE TABLE IF NOT EXISTS logical_runs(id TEXT PRIMARY KEY,pr TEXT NOT NULL,plan TEXT NOT NULL,limits TEXT NOT NULL,review_allowance INTEGER NOT NULL,status TEXT NOT NULL DEFAULT 'active');
CREATE UNIQUE INDEX IF NOT EXISTS one_active_pr ON logical_runs(pr) WHERE status='active';
CREATE TABLE IF NOT EXISTS executions(run TEXT NOT NULL REFERENCES logical_runs(id),request TEXT NOT NULL,engine TEXT NOT NULL DEFAULT '',plan TEXT NOT NULL,PRIMARY KEY(run,request));
CREATE TABLE IF NOT EXISTS admissions(run TEXT NOT NULL REFERENCES logical_runs(id),request TEXT NOT NULL,plan TEXT NOT NULL,fingerprint TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(run,request));
CREATE TABLE IF NOT EXISTS reservations(run TEXT NOT NULL REFERENCES logical_runs(id),kind TEXT NOT NULL,scope TEXT NOT NULL,operation TEXT NOT NULL,reason TEXT NOT NULL,PRIMARY KEY(run,kind,scope,operation));
CREATE TABLE IF NOT EXISTS continuations(run TEXT NOT NULL REFERENCES logical_runs(id),request TEXT NOT NULL,instruction TEXT NOT NULL,source TEXT NOT NULL,cycles INTEGER NOT NULL,allowance INTEGER NOT NULL,PRIMARY KEY(run,request));
CREATE TABLE IF NOT EXISTS claims(run TEXT NOT NULL REFERENCES logical_runs(id),id TEXT NOT NULL,occurrence TEXT NOT NULL,fingerprint TEXT NOT NULL,cycle TEXT NOT NULL,engine TEXT NOT NULL,worker TEXT NOT NULL DEFAULT '',message TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,checks INTEGER NOT NULL DEFAULT 0,report_message TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',snapshot TEXT NOT NULL,assignment TEXT NOT NULL,payload TEXT NOT NULL DEFAULT '',operation TEXT NOT NULL,PRIMARY KEY(run,id));
CREATE UNIQUE INDEX IF NOT EXISTS claim_operation ON claims(run,operation);
CREATE TABLE IF NOT EXISTS local_effects(run TEXT NOT NULL,claim TEXT NOT NULL,parent TEXT NOT NULL,tree TEXT NOT NULL DEFAULT '',message TEXT NOT NULL,head TEXT NOT NULL DEFAULT '',PRIMARY KEY(run,claim));
CREATE INDEX IF NOT EXISTS claim_occurrence ON claims(run,occurrence,fingerprint);
CREATE TABLE IF NOT EXISTS submissions(run TEXT NOT NULL,claim TEXT NOT NULL,round INTEGER NOT NULL,message TEXT NOT NULL,status TEXT NOT NULL,PRIMARY KEY(run,claim,round),FOREIGN KEY(run,claim) REFERENCES claims(run,id));
CREATE TABLE IF NOT EXISTS effects(run TEXT NOT NULL REFERENCES logical_runs(id),id TEXT NOT NULL,intent TEXT NOT NULL,state TEXT NOT NULL,receipt TEXT NOT NULL DEFAULT '',PRIMARY KEY(run,id));
CREATE TABLE IF NOT EXISTS effect_aliases(run TEXT NOT NULL REFERENCES logical_runs(id),id TEXT NOT NULL,canonical_run TEXT NOT NULL,canonical_id TEXT NOT NULL,proposal TEXT NOT NULL,PRIMARY KEY(run,id),FOREIGN KEY(canonical_run,canonical_id) REFERENCES effects(run,id));
CREATE TABLE IF NOT EXISTS reply_replacements(original_run TEXT NOT NULL,original_id TEXT NOT NULL,observation TEXT NOT NULL,replacement_run TEXT NOT NULL,replacement_id TEXT NOT NULL,PRIMARY KEY(original_run,original_id,observation),FOREIGN KEY(replacement_run,replacement_id) REFERENCES effects(run,id));
CREATE TABLE IF NOT EXISTS review_request_dispositions(run TEXT NOT NULL,id TEXT NOT NULL,artifact TEXT NOT NULL,PRIMARY KEY(run,id),FOREIGN KEY(run,id) REFERENCES effects(run,id));
CREATE TABLE IF NOT EXISTS ci_windows(run TEXT NOT NULL REFERENCES logical_runs(id),head TEXT NOT NULL,operation TEXT NOT NULL,started TEXT NOT NULL,PRIMARY KEY(run,operation));
CREATE TABLE IF NOT EXISTS deadlines(run TEXT NOT NULL REFERENCES logical_runs(id),head TEXT NOT NULL,deadline TEXT NOT NULL,started TEXT NOT NULL,extensions INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(run,head));
CREATE TABLE IF NOT EXISTS progress(run TEXT NOT NULL REFERENCES logical_runs(id),id INTEGER PRIMARY KEY AUTOINCREMENT,source TEXT NOT NULL,kind TEXT NOT NULL,reference TEXT NOT NULL);
`

func OpenLedger(root string) (*Ledger, error) {
	dir := filepath.Join(root, ".agent-workflow", "pr-fix")
	if e := safeParent(filepath.Join(dir, "ledger.sqlite")); e != nil {
		return nil, e
	}
	if e := os.Chmod(dir, 0700); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, "ledger.sqlite")
	if st, e := os.Lstat(path); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("ledger cannot be a symlink")
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	db, e := sql.Open("sqlite", dsn)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	l := &Ledger{db, root}
	if _, e = db.Exec(ledgerSchema); e != nil {
		db.Close()
		return nil, e
	}
	if e = os.Chmod(path, 0600); e != nil {
		db.Close()
		return nil, e
	}
	return l, nil
}
func (l *Ledger) Close() error { return l.db.Close() }

// Recovery observes an existing ledger without initializing schema or repairing
// permissions. A missing ledger cannot establish the outcome of an old action.
func openLedgerReadOnly(ctx context.Context, root string) (*Ledger, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".agent-workflow", "pr-fix", "ledger.sqlite")
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	for probe := filepath.Dir(abs); ; probe = filepath.Dir(probe) {
		st, err := os.Lstat(probe)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return nil, fmt.Errorf("application directory must be a real directory: %s", probe)
		}
		if filepath.Dir(probe) == probe {
			break
		}
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("ledger must be a regular file")
	}
	dsn := (&url.URL{Scheme: "file", Path: abs}).String() + "?mode=ro&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Ledger{db, root}, nil
}

type sqlRW interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (l *Ledger) transaction(ctx context.Context, fn func(sqlRW) error) (err error) {
	conn, e := l.db.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	if _, e = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); e != nil {
		return e
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return
}
func (l *Ledger) SavePlan(ctx context.Context, p Plan) (string, error) {
	path, e := artifact(l.Root, p)
	if e != nil {
		return "", e
	}
	limits, _ := json.Marshal(p.Selection.Limits())
	e = l.transaction(ctx, func(tx sqlRW) error {
		var oldPath, pr, status string
		var oldLimits Limits
		var raw string
		e := tx.QueryRowContext(ctx, "SELECT pr,plan,limits,status FROM logical_runs WHERE id=?", p.LogicalRun).Scan(&pr, &oldPath, &raw, &status)
		if errors.Is(e, sql.ErrNoRows) {
			_, e = tx.ExecContext(ctx, "INSERT INTO logical_runs(id,pr,plan,limits,review_allowance) VALUES(?,?,?,?,?)", p.LogicalRun, p.Identity.URL, path, string(limits), p.Selection.Limits().ReviewCycles)
			return e
		}
		if e != nil {
			return e
		}
		if pr != p.Identity.URL || (status != "active" && status != "complete") {
			return fmt.Errorf("logical run identity/status conflict")
		}
		if e = json.Unmarshal([]byte(raw), &oldLimits); e != nil {
			return e
		}
		n := p.Selection.Limits()
		n.ReviewCycles = min(n.ReviewCycles, oldLimits.ReviewCycles)
		n.Retries = min(n.Retries, oldLimits.Retries)
		n.CIRepairs = min(n.CIRepairs, oldLimits.CIRepairs)
		n.CIReruns = min(n.CIReruns, oldLimits.CIReruns)
		if oldPath != path {
			var pending int
			if e = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM claims WHERE run=? AND state IN ('claimed','recheck','recover_required'))+(SELECT count(*) FROM effects WHERE run=? AND state IN ('dispatched','unknown'))", p.LogicalRun, p.LogicalRun).Scan(&pending); e != nil {
				return e
			}
			if pending > 0 {
				return fmt.Errorf("reconcile %d unfinished claims/effects before replacing execution", pending)
			}
			var engine string
			e = tx.QueryRowContext(ctx, "SELECT engine FROM executions WHERE run=? ORDER BY rowid DESC LIMIT 1", p.LogicalRun).Scan(&engine)
			if e == nil {
				if engine == "" {
					return fmt.Errorf("reconcile lost engine admission response before replacing execution")
				}
				store, e := workflows.OpenSQLiteInspection(filepath.Join(l.Root, ".agent-workflow"))
				if e != nil {
					return e
				}
				run, _, e := store.Load(ctx, engine)
				store.Close()
				if e != nil {
					return e
				}
				if run.Status == workflows.RunStatusQueued || run.Status == workflows.RunStatusRunning || run.Status == workflows.RunStatusWaiting || run.Status == workflows.RunStatusInterrupted {
					return fmt.Errorf("execution %s is still %s; settle it before replacing the plan", engine, run.Status)
				}
			} else if !errors.Is(e, sql.ErrNoRows) {
				return e
			}

		}
		b, _ := json.Marshal(n)
		_, e = tx.ExecContext(ctx, "UPDATE logical_runs SET plan=?,limits=?,review_allowance=review_allowance-?,status=CASE WHEN plan<>? THEN 'active' ELSE status END WHERE id=?", path, string(b), oldLimits.ReviewCycles-n.ReviewCycles, path, p.LogicalRun)
		return e
	})
	return path, e
}
func (l *Ledger) Plan(ctx context.Context, id string) (Plan, error) {
	var path string
	e := l.db.QueryRowContext(ctx, "SELECT plan FROM logical_runs WHERE id=?", id).Scan(&path)
	var p Plan
	if e == nil {
		e = readJSON(path, &p)
	}
	return p, e
}
func (l *Ledger) Active(ctx context.Context, pr string) (string, error) {
	var id string
	e := l.db.QueryRowContext(ctx, "SELECT id FROM logical_runs WHERE pr=? ORDER BY rowid DESC LIMIT 1", pr).Scan(&id)
	return id, e
}
func reserve(ctx context.Context, tx sqlRW, run, kind, scope, operation, reason string) error {
	var prev string
	e := tx.QueryRowContext(ctx, "SELECT reason FROM reservations WHERE run=? AND kind=? AND scope=? AND operation=?", run, kind, scope, operation).Scan(&prev)
	if e == nil {
		if prev != reason {
			return fmt.Errorf("conflicting budget reservation replay")
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	var raw string
	var allowance int
	if e = tx.QueryRowContext(ctx, "SELECT limits,review_allowance FROM logical_runs WHERE id=?", run).Scan(&raw, &allowance); e != nil {
		return e
	}
	var limits Limits
	if e = json.Unmarshal([]byte(raw), &limits); e != nil {
		return e
	}
	ceiling := 0
	switch kind {
	case "review":
		ceiling = allowance
	case "retry":
		ceiling = limits.Retries
	case "ci_repair":
		ceiling = limits.CIRepairs
	case "ci_rerun":
		ceiling = limits.CIReruns
	default:
		return fmt.Errorf("unknown budget kind %q", kind)
	}
	if !validText(operation) || !validText(scope) || !validText(reason) {
		return fmt.Errorf("reservation identity and reason required")
	}
	var used int
	if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM reservations WHERE run=? AND kind=? AND scope=?", run, kind, scope).Scan(&used); e != nil {
		return e
	}
	if used >= ceiling {
		return fmt.Errorf("%s budget exhausted (%d/%d); retained state requires renewed authorization for review continuation", kind, used, ceiling)
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO reservations VALUES(?,?,?,?,?)", run, kind, scope, operation, reason)
	return e
}
func (l *Ledger) Reserve(ctx context.Context, run, kind, scope, operation, reason string) error {
	return l.transaction(ctx, func(tx sqlRW) error { return reserve(ctx, tx, run, kind, scope, operation, reason) })
}
func (l *Ledger) Continue(ctx context.Context, run string, a Authorization, cycles int) (int, error) {
	if e := a.Validate(); e != nil {
		return 0, e
	}
	if cycles < 1 || cycles > 5 {
		return 0, fmt.Errorf("continuation grants one to five cycles")
	}
	var total int
	e := l.transaction(ctx, func(tx sqlRW) error {
		var instruction, source string
		var n int
		e := tx.QueryRowContext(ctx, "SELECT instruction,source,cycles,allowance FROM continuations WHERE run=? AND request=?", run, a.RequestID).Scan(&instruction, &source, &n, &total)
		if e == nil {
			if instruction != a.Instruction || source != a.Source || n != cycles {
				return fmt.Errorf("conflicting continuation replay")
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var used int
		if e = tx.QueryRowContext(ctx, "SELECT review_allowance,(SELECT count(*) FROM reservations WHERE run=? AND kind='review') FROM logical_runs WHERE id=?", run, run).Scan(&total, &used); e != nil {
			return e
		}
		if used < total {
			return fmt.Errorf("review allowance not exhausted")
		}
		total += cycles
		if _, e = tx.ExecContext(ctx, "INSERT INTO continuations VALUES(?,?,?,?,?,?)", run, a.RequestID, a.Instruction, a.Source, cycles, total); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE logical_runs SET review_allowance=? WHERE id=?", total, run)
		return e
	})
	return total, e
}
func (l *Ledger) Deadline(ctx context.Context, run, head string, now time.Time, timeout time.Duration) (time.Time, error) {
	var result time.Time
	e := l.transaction(ctx, func(tx sqlRW) error {
		scope, started, e := deadlineScope(ctx, tx, run, head)
		if e != nil {
			return e
		}
		if !started.IsZero() {
			now = started
		}
		var raw string
		e = tx.QueryRowContext(ctx, "SELECT deadline FROM deadlines WHERE run=? AND head=?", run, scope).Scan(&raw)
		if e == nil {
			result, e = time.Parse(time.RFC3339Nano, raw)
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		result = now.Add(timeout)
		_, e = tx.ExecContext(ctx, "INSERT INTO deadlines(run,head,deadline,started) VALUES(?,?,?,?)", run, scope, result.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		return e
	})
	return result, e
}
func (l *Ledger) Execution(ctx context.Context, run, request, plan, engine string) error {
	return l.transaction(ctx, func(tx sqlRW) error {
		if _, e := tx.ExecContext(ctx, "UPDATE admissions SET state='admitted' WHERE run=? AND request=?", run, request); e != nil {
			return e
		}
		var p, id string
		e := tx.QueryRowContext(ctx, "SELECT plan,engine FROM executions WHERE run=? AND request=?", run, request).Scan(&p, &id)
		if e == nil {
			if p != plan || id != "" && engine != "" && id != engine {
				return fmt.Errorf("execution request conflict")
			}
			if engine == "" {
				return nil
			}
			_, e = tx.ExecContext(ctx, "UPDATE executions SET engine=? WHERE run=? AND request=?", engine, run, request)
			return e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO executions VALUES(?,?,?,?)", run, request, engine, plan)
		return e
	})
}
func (l *Ledger) LatestExecution(ctx context.Context, run string) (string, string, error) {
	var request, engine string
	e := l.db.QueryRowContext(ctx, "SELECT request,engine FROM executions WHERE run=? ORDER BY rowid DESC LIMIT 1", run).Scan(&request, &engine)
	return request, engine, e
}

func (l *Ledger) Admission(ctx context.Context, run, plan, fingerprint, requested string) (string, error) {
	var id string
	e := l.transaction(ctx, func(tx sqlRW) error {
		if requested != "" {
			var oldPlan string
			e := tx.QueryRowContext(ctx, "SELECT plan FROM admissions WHERE run=? AND request=?", run, requested).Scan(&oldPlan)
			if e == nil {
				if oldPlan != plan {
					return fmt.Errorf("admission request changed plan")
				}
				id = requested
				return nil
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		var oldID, oldPlan, oldFP, state string
		e := tx.QueryRowContext(ctx, "SELECT request,plan,fingerprint,state FROM admissions WHERE run=? AND state IN ('prepared','admitted') ORDER BY rowid DESC LIMIT 1", run).Scan(&oldID, &oldPlan, &oldFP, &state)
		if e == nil && oldFP == fingerprint && requested == "" {
			id = oldID
			return nil
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if oldID != "" {
			if state == "admitted" {
				var engine string
				if e = tx.QueryRowContext(ctx, "SELECT engine FROM executions WHERE run=? AND request=?", run, oldID).Scan(&engine); e != nil {
					return fmt.Errorf("reconcile original admission before replacement: %w", e)
				}
				if e = terminalEngine(ctx, l.Root, engine); e != nil {
					return e
				}
				var pending int
				if e = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM claims WHERE run=? AND state IN ('claimed','recheck','recover_required'))+(SELECT count(*) FROM effects WHERE run=? AND state IN ('dispatched','unknown'))", run, run).Scan(&pending); e != nil {
					return e
				}
				if pending > 0 {
					return fmt.Errorf("unfinished admission requires original reconciliation")
				}
			}
			if _, e = tx.ExecContext(ctx, "UPDATE admissions SET state='superseded' WHERE run=? AND request=?", run, oldID); e != nil {
				return e
			}
		}
		id = requested
		if id == "" {
			id = "prfix-" + newID()
		}
		if _, e = tx.ExecContext(ctx, "UPDATE logical_runs SET status='active' WHERE id=?", run); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO admissions VALUES(?,?,?,?,'prepared')", run, id, plan, fingerprint)
		return e
	})
	return id, e
}

func (l *Ledger) Started(ctx context.Context, run, head string) (time.Time, error) {
	scope, _, e := deadlineScope(ctx, l.db, run, head)
	if e != nil {
		return time.Time{}, e
	}
	var raw string
	e = l.db.QueryRowContext(ctx, "SELECT started FROM deadlines WHERE run=? AND head=?", run, scope).Scan(&raw)
	if e != nil {
		return time.Time{}, e
	}
	return time.Parse(time.RFC3339Nano, raw)
}
func (l *Ledger) Extend(ctx context.Context, run, head string, extension time.Duration, maximum int) (time.Time, bool, error) {
	var deadline time.Time
	extended := false
	e := l.transaction(ctx, func(tx sqlRW) error {
		scope, _, e := deadlineScope(ctx, tx, run, head)
		if e != nil {
			return e
		}
		var raw string
		var used int
		if e := tx.QueryRowContext(ctx, "SELECT deadline,extensions FROM deadlines WHERE run=? AND head=?", run, scope).Scan(&raw, &used); e != nil {
			return e
		}
		deadline, e = time.Parse(time.RFC3339Nano, raw)
		if e != nil {
			return e
		}
		if used >= min(1, maximum) || extension <= 0 {
			return nil
		}
		deadline = deadline.Add(extension)
		_, e = tx.ExecContext(ctx, "UPDATE deadlines SET deadline=?,extensions=extensions+1 WHERE run=? AND head=?", deadline.Format(time.RFC3339Nano), run, scope)
		extended = e == nil
		return e
	})
	return deadline, extended, e
}

// Verified reruns have distinct immutable windows. The original head window and
// its consumed extension remain retained; replaying a receipt cannot restart one.
func deadlineScope(ctx context.Context, db sqlRW, run, head string) (string, time.Time, error) {
	var operation, raw string
	e := db.QueryRowContext(ctx, "SELECT operation,started FROM ci_windows WHERE run=? AND head=? ORDER BY rowid DESC LIMIT 1", run, head).Scan(&operation, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return head, time.Time{}, nil
	}
	if e != nil {
		return "", time.Time{}, e
	}
	started, e := time.Parse(time.RFC3339Nano, raw)
	return head + ":rerun:" + operation, started, e
}
