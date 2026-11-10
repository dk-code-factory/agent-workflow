package prfix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// A separate application-owned SQLite lock serializes dispatch and observation
// across controllers and processes, without holding the bookkeeping DB's writer
// lock during network operations. Process exit releases the lock automatically.
func (r *Runtime) lockPublication(ctx context.Context) (func(), error) {
	path := filepath.Join(r.Root, ".agent-workflow", "pr-fix", "publication-lock.sqlite")
	if err := safeParent(path); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		return nil, fmt.Errorf("publication lock must be a regular file")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout(0)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	fail := func(err error) (func(), error) { conn.Close(); db.Close(); return nil, err }
	for {
		_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil {
			break
		}
		var coded interface{ Code() int }
		if !errors.As(err, &coded) || (coded.Code()&255) != 5 && (coded.Code()&255) != 6 {
			return fail(err)
		}
		if r.publicationContended != nil {
			r.publicationContended()
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fail(ctx.Err())
		case <-timer.C:
		}
	}
	if err = os.Chmod(path, 0600); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return fail(err)
	}
	return func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK"); _ = conn.Close(); _ = db.Close() }, nil
}
