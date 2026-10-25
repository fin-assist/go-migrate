package migrate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	engine "github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeQueue struct {
	exec    func(context.Context, string, ...any) error
	closed  int
	queries []string
}

func (q *fakeQueue) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.queries = append(q.queries, sql)
	if q.exec != nil {
		return pgconn.CommandTag{}, q.exec(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}
func (q *fakeQueue) Close(context.Context) error { q.closed++; return nil }

type fakeSession struct {
	v                           uint
	dirty                       bool
	upErr, forceErr, versionErr error
	steps                       func(int) error
	closed                      int
}

func (m *fakeSession) Steps(n int) error {
	if m.steps != nil {
		return m.steps(n)
	}
	if n < 0 {
		if m.v == 0 {
			return os.ErrNotExist
		}
		m.v--
	} else {
		if m.upErr != nil {
			return m.upErr
		}
		if m.v >= 3 {
			return os.ErrNotExist
		}
		m.v++
	}
	return nil
}
func (m *fakeSession) Force(v int) error {
	if m.forceErr == nil {
		m.v = uint(v)
		m.dirty = false
	}
	return m.forceErr
}
func (m *fakeSession) Version() (uint, bool, error) { return m.v, m.dirty, m.versionErr }
func (m *fakeSession) Close() (error, error)        { m.closed++; return nil, nil }

func source() Source {
	return Source{Name: "service", Table: "schema_migrations", FS: fstest.MapFS{"000001_a.up.sql": {Data: []byte("SELECT 1")}, "000001_a.down.sql": {Data: []byte("SELECT 1")}}}
}
func fakeRunner(q *fakeQueue, m *fakeSession) runner {
	return runner{connect: func(context.Context, *pgx.ConnConfig) (queueConn, error) { return q, nil }, open: func(Config, Source) (migrationSession, error) { return m, nil }}
}
func TestUnitRunnerLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig("postgres://localhost/db")
	src := source()
	for _, operation := range []string{"up", "noop", "versions", "nilversion", "down", "force", "up-error", "version-error", "down-error", "force-error"} {
		t.Run(operation, func(t *testing.T) {
			q := &fakeQueue{}
			m := &fakeSession{v: 2}
			r := fakeRunner(q, m)
			sentinel := errors.New("engine failure")
			var err error
			switch operation {
			case "up":
				err = r.up(ctx, cfg, []Source{src})
				if m.v != 3 {
					t.Fatal(m.v)
				}
			case "noop":
				m.upErr = engine.ErrNoChange
				err = r.up(ctx, cfg, []Source{src})
			case "versions":
				m.dirty = true
				var st []Status
				st, err = r.versions(ctx, cfg, []Source{src})
				if len(st) != 1 || st[0].Version != 2 || !st[0].Dirty {
					t.Fatal(st)
				}
			case "nilversion":
				m.versionErr = engine.ErrNilVersion
				var st []Status
				st, err = r.versions(ctx, cfg, []Source{src})
				if len(st) != 1 || st[0].Version != 0 {
					t.Fatal(st)
				}
			case "down":
				err = r.down(ctx, cfg, src, 1)
				if m.v != 1 {
					t.Fatal(m.v)
				}
			case "force":
				m.dirty = true
				err = r.force(ctx, cfg, src, 7)
				if m.v != 7 || m.dirty {
					t.Fatal(m)
				}
			case "up-error":
				m.upErr = sentinel
				err = r.up(ctx, cfg, []Source{src})
			case "version-error":
				m.versionErr = sentinel
				_, err = r.versions(ctx, cfg, []Source{src})
			case "down-error":
				m.steps = func(int) error { return sentinel }
				err = r.down(ctx, cfg, src, 1)
			case "force-error":
				m.forceErr = sentinel
				err = r.force(ctx, cfg, src, 1)
			}
			if strings.HasSuffix(operation, "-error") {
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if m.closed != 1 || q.closed != 1 || len(q.queries) != 2 {
				t.Fatalf("cleanup session=%d queue=%d queries=%v", m.closed, q.closed, q.queries)
			}
		})
	}
}
func TestUnitDownPreservesShortLimit(t *testing.T) {
	q := &fakeQueue{}
	m := &fakeSession{v: 2}
	r := fakeRunner(q, m)
	err := r.down(context.Background(), DefaultConfig("postgres://localhost/db"), source(), 4)
	var short engine.ErrShortLimit
	if !errors.As(err, &short) || short.Short != 2 || m.v != 0 || m.closed != 1 || q.closed != 1 {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestUnitQueueConfigurationAndFailures(t *testing.T) {
	cfg := DefaultConfig("postgres://localhost/db?application_name=old&lock_timeout=900")
	cfg.WaitTimeout = 20 * time.Millisecond
	src := source()
	sentinel := errors.New("connection failure")
	for _, mode := range []string{"success", "connect", "lock", "timeout", "cancel", "parse", "open", "empty"} {
		t.Run(mode, func(t *testing.T) {
			q := &fakeQueue{}
			m := &fakeSession{}
			r := fakeRunner(q, m)
			r.connect = func(ctx context.Context, pc *pgx.ConnConfig) (queueConn, error) {
				if pc.RuntimeParams["application_name"] != "migrate-wait" || pc.RuntimeParams["lock_timeout"] != "0" || pc.RuntimeParams["statement_timeout"] != "0" {
					t.Fatal(pc.RuntimeParams)
				}
				if mode == "connect" {
					return nil, sentinel
				}
				return q, nil
			}
			q.exec = func(ctx context.Context, sql string, args ...any) error {
				if len(args) != 1 || args[0] != "fin-assist/migrate/schema_migrations" {
					t.Fatalf("arguments: %#v", args)
				}
				if strings.Contains(sql, "unlock") {
					if ctx.Err() != nil {
						t.Fatal(ctx.Err())
					}
					return nil
				}
				if mode == "lock" || mode == "cancel" {
					return sentinel
				}
				if mode == "timeout" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := cfg
			if mode == "parse" {
				c.DSN = "://"
			}
			if mode == "cancel" {
				cancel()
			}
			if mode == "open" || mode == "empty" {
				r.open = func(Config, Source) (migrationSession, error) {
					if mode == "empty" {
						return nil, errEmpty
					}
					return nil, sentinel
				}
			}
			err := r.up(ctx, c, []Source{src})
			if mode == "success" || mode == "empty" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("missing error")
			}
			if mode == "timeout" && !strings.Contains(err.Error(), "still runs after") {
				t.Fatal(err)
			}
			if mode != "parse" && mode != "connect" && q.closed != 1 {
				t.Fatalf("queue not closed: %d", q.closed)
			}
		})
	}
}
func TestUnitCancellationStopsBeforeNextFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &fakeQueue{}
	m := &fakeSession{}
	r := fakeRunner(q, m)
	m.steps = func(int) error { m.v++; cancel(); return nil }
	err := r.up(ctx, DefaultConfig("postgres://localhost/db"), []Source{source()})
	if !errors.Is(err, context.Canceled) || m.v != 1 || q.closed != 1 || m.closed != 1 {
		t.Fatalf("%v %+v %+v", err, q, m)
	}
	// A genuine SQL failure must win over simultaneous cancellation.
	ctx, cancel = context.WithCancel(context.Background())
	sentinel := errors.New("SQL failure")
	m.steps = func(int) error { cancel(); return sentinel }
	if err := r.up(ctx, DefaultConfig("postgres://localhost/db"), []Source{source()}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

type brokenFS struct{}

func (brokenFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }
func TestUnitValidationAndConfiguration(t *testing.T) {
	src := source()
	ctx := context.Background()
	cfg := DefaultConfig("postgres://localhost/db")
	bad := []Source{{}, {Name: "a", Table: "t", FS: brokenFS{}}}
	r := runner{connect: func(context.Context, *pgx.ConnConfig) (queueConn, error) {
		t.Fatal("validation reached database")
		return nil, nil
	}}
	for _, s := range bad {
		if err := r.up(ctx, cfg, []Source{s}); err == nil {
			t.Fatal("up")
		}
		if _, err := r.versions(ctx, cfg, []Source{s}); err == nil {
			t.Fatal("versions")
		}
		if err := r.down(ctx, cfg, s, 1); err == nil {
			t.Fatal("down")
		}
		if err := r.force(ctx, cfg, s, 0); err == nil {
			t.Fatal("force")
		}
		if err := r.check(ctx, cfg, s); err == nil {
			t.Fatal("check")
		}
	}
	if err := r.down(ctx, cfg, src, 0); err == nil {
		t.Fatal("nonpositive down")
	}
	if err := r.up(ctx, cfg, []Source{src, src}); err == nil {
		t.Fatal("duplicate")
	}
	if _, err := open(Config{DSN: "://"}); err == nil {
		t.Fatal("bad DSN")
	}
	db, err := open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if millis(1500*time.Millisecond) != "1500" || !empty(fstest.MapFS{}) || empty(src.FS) || empty(brokenFS{}) {
		t.Fatal("source classification")
	}
	cfg.Logger = nil
	if err := fakeRunner(&fakeQueue{}, &fakeSession{}).up(ctx, cfg, []Source{src}); err != nil {
		t.Fatal(err)
	}

}
