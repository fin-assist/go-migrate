// Package migrate applies the SQL migrations of the service and of the libraries it embeds
// (idempotency, outbox, tombstone, ...) with golang-migrate.
//
// Every source has its own version table, so version numbers of a library never collide
// with the service's own. Sources are applied in the order given: libraries first, then
// the service. Runs of one source are serialised by advisory locks (see waitTurn), so two
// concurrent runs (two Jobs, a Job and a deploy script) wait for each other instead of
// applying the same migration twice.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/fin-assist/go-migrate/internal/safeerror"
)

// Source is one set of migrations with its own version table.
type Source struct {
	// Name identifies the source in logs and on the command line ("service", "outbox", ...).
	Name string
	// Table is the version table of the source. Must be unique across sources.
	Table string
	// FS holds NNN_name.up.sql / NNN_name.down.sql files at its root.
	FS fs.FS
}

// Config of a migration run.
type Config struct {
	// DSN of the primary. Migrations never run against a replica.
	DSN string
	// LockTimeout is the session lock_timeout: a DDL statement that waits longer for a lock
	// on a busy table fails instead of blocking the traffic queued behind it.
	LockTimeout time.Duration
	// StatementTimeout is the session statement_timeout for every statement of a migration.
	StatementTimeout time.Duration
	// WaitTimeout is how long to wait for another run holding the migration lock of a source.
	WaitTimeout time.Duration
	// Logger receives operator events; nil discards them.
	Logger *slog.Logger
}

// DefaultConfig returns the recommended timeouts and a discard logger.
// The caller owns configuration and may replace Logger with its service logger.
func DefaultConfig(dsn string) Config {
	return Config{
		DSN:              dsn,
		LockTimeout:      5 * time.Second,
		StatementTimeout: 15 * time.Minute,
		WaitTimeout:      15 * time.Minute,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Status of one source.
type Status struct {
	Source  string
	Version uint // 0 when nothing is applied
	Dirty   bool
}

// ErrInvalidDSN means that connection configuration could not be parsed.
// The unsafe original ParseError, which may contain credentials, is not retained.
var ErrInvalidDSN = errors.New("invalid migration connection configuration")

// errEmpty: the source has no migration files yet (a fresh service from the template).
var errEmpty = errors.New("no migration files")

var fileName = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

// Validate checks the files of a source: every name matches NNN_name.(up|down).sql, every
// version has exactly one up and one down file, and versions are unique. A down file is
// mandatory (FIN-115): an irreversible step still has one, which fails with an explanation.
func Validate(src Source) error {
	if src.FS == nil {
		return fmt.Errorf("source %s: files are required", src.Name)
	}
	entries, err := fs.ReadDir(src.FS, ".")
	if err != nil {
		return fmt.Errorf("source %s: %w", src.Name, err)
	}
	type pair struct {
		name     string
		up, down bool
	}
	versions := map[uint64]*pair{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "embed.go" || !isSQL(e.Name()) {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return fmt.Errorf("source %s: %q does not match NNN_name.up.sql / NNN_name.down.sql", src.Name, e.Name())
		}
		v, _ := strconv.ParseUint(m[1], 10, 64)
		p := versions[v]
		if p == nil {
			p = &pair{name: m[2]}
			versions[v] = p
		} else if p.name != m[2] {
			return fmt.Errorf("source %s: version %d is used by %q and %q", src.Name, v, p.name, m[2])
		}
		switch m[3] {
		case "up":
			p.up = true
		case "down":
			p.down = true
		}
	}
	for v, p := range versions {
		if !p.up {
			return fmt.Errorf("source %s: version %d (%s) has no up file", src.Name, v, p.name)
		}
		if !p.down {
			return fmt.Errorf("source %s: version %d (%s) has no down file", src.Name, v, p.name)
		}
	}
	return nil
}

func isSQL(name string) bool {
	return len(name) > 4 && name[len(name)-4:] == ".sql"
}

func validateAll(sources []Source) error {
	names, tables := map[string]bool{}, map[string]bool{}
	for _, s := range sources {
		if s.Name == "" || s.Table == "" || s.FS == nil {
			return fmt.Errorf("source %q: name, table and files are required", s.Name)
		}
		if names[s.Name] {
			return fmt.Errorf("source %q is listed twice", s.Name)
		}
		if tables[s.Table] {
			return fmt.Errorf("version table %q is used by two sources", s.Table)
		}
		names[s.Name], tables[s.Table] = true, true
		if err := Validate(s); err != nil {
			return err
		}
	}
	return nil
}

// Up applies all pending migrations of every source, in order. It stops at the first
// failure; a failed SQL file leaves its source dirty until checked and repaired by an operator.
func Up(ctx context.Context, cfg Config, sources []Source) error {
	return production().up(ctx, cfg, sources)
}

func (r runner) up(ctx context.Context, cfg Config, sources []Source) error {
	if err := validateAll(sources); err != nil {
		return err
	}
	for _, s := range sources {
		err := r.withMigrate(ctx, cfg, s, func(m migrationSession) error {
			before, _, _ := m.Version()
			applied := false
			for {
				err := step(ctx, m, 1)
				if errors.Is(err, migrate.ErrNoChange) || errors.Is(err, os.ErrNotExist) {
					if !applied {
						cfg.logger().InfoContext(ctx, "migrations up to date", "source", s.Name, "version", before)
						return nil
					}
					break
				}
				if err != nil {
					return err
				}
				applied = true
			}
			after, _, _ := m.Version()
			cfg.logger().InfoContext(ctx, "migrations applied", "source", s.Name, "from", before, "to", after)
			return nil
		})
		if errors.Is(err, errEmpty) {
			cfg.logger().InfoContext(ctx, "no migrations", "source", s.Name)
			continue
		}
		if err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
	}
	return nil
}

// Versions reports the applied version of every source.
func Versions(ctx context.Context, cfg Config, sources []Source) ([]Status, error) {
	return production().versions(ctx, cfg, sources)
}

func (r runner) versions(ctx context.Context, cfg Config, sources []Source) ([]Status, error) {
	if err := validateAll(sources); err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(sources))
	for _, s := range sources {
		st := Status{Source: s.Name}
		err := r.withMigrate(ctx, cfg, s, func(m migrationSession) error {
			v, dirty, err := m.Version()
			if errors.Is(err, migrate.ErrNilVersion) {
				return nil
			}
			st.Version, st.Dirty = v, dirty
			return safeerror.Wrap("read migration version failed", err)
		})
		if err != nil && !errors.Is(err, errEmpty) {
			return nil, fmt.Errorf("source %s: %w", s.Name, err)
		}
		out = append(out, st)
	}
	return out, nil
}

// Down rolls back the last steps migrations of one source. Used for a failed "add" step
// after the application has been rolled back; never part of a regular deploy.
func Down(ctx context.Context, cfg Config, src Source, steps int) error {
	return production().down(ctx, cfg, src, steps)
}

func (r runner) down(ctx context.Context, cfg Config, src Source, steps int) error {
	if steps <= 0 {
		return fmt.Errorf("steps must be positive, got %d", steps)
	}
	if err := validateAll([]Source{src}); err != nil {
		return err
	}
	return r.withMigrate(ctx, cfg, src, func(m migrationSession) error {
		for i := 0; i < steps; i++ {
			if err := step(ctx, m, -1); err != nil {
				if i > 0 && errors.Is(err, os.ErrNotExist) {
					return migrate.ErrShortLimit{Short: uint(steps - i)}
				}
				return err
			}
		}
		v, _, _ := m.Version()
		cfg.logger().WarnContext(ctx, "migrations rolled back", "source", src.Name, "steps", steps, "version", v)
		return nil
	})
}

// Force sets the version of a source and clears the dirty flag without running any SQL.
// Only call it after the actual schema has been checked by hand.
func Force(ctx context.Context, cfg Config, src Source, version int) error {
	return production().force(ctx, cfg, src, version)
}

func (r runner) force(ctx context.Context, cfg Config, src Source, version int) error {
	if err := validateAll([]Source{src}); err != nil {
		return err
	}
	return r.withMigrate(ctx, cfg, src, func(m migrationSession) error {
		if err := m.Force(version); err != nil {
			return safeerror.Wrap("force migration version failed", err)
		}
		cfg.logger().WarnContext(ctx, "migration version forced", "source", src.Name, "version", version)
		return nil
	})
}

// migrationSession is the migration engine consumed by the orchestration layer.
type migrationSession interface {
	Steps(int) error
	Force(int) error
	Version() (uint, bool, error)
	Close() (error, error)
}
type queueConn interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Close(context.Context) error
}
type runner struct {
	connect func(context.Context, *pgx.ConnConfig) (queueConn, error)
	open    func(Config, Source) (migrationSession, error)
}

func production() runner {
	return runner{connect: func(ctx context.Context, pc *pgx.ConnConfig) (queueConn, error) {
		return pgx.ConnectConfig(ctx, pc)
	}, open: openMigration}
}

// withMigrate keeps the queue connection alive until the engine and its pool close.
func (r runner) withMigrate(ctx context.Context, cfg Config, src Source, fn func(migrationSession) error) error {
	release, err := r.waitTurn(ctx, cfg, src)
	if err != nil {
		return err
	}
	defer release()
	m, err := r.open(cfg, src)
	if err != nil {
		if errors.Is(err, errEmpty) && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	err = fn(m)
	if err == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// step checks cancellation between complete SQL files, avoiding upstream GracefulStop's
// unsynchronised shared state. The queue lock remains held across all steps.
func step(ctx context.Context, m migrationSession, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := m.Steps(n)
	if err == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return safeerror.Wrap("migration step failed", err)
}

func openMigration(cfg Config, src Source) (migrationSession, error) {
	owned := &openingConnections{}
	db, err := open(cfg, stdlib.OptionAfterConnect(func(_ context.Context, c *pgx.Conn) error { owned.capture(c); return nil }))
	if err != nil {
		return nil, err
	}
	drv, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{MigrationsTable: src.Table})
	if err != nil {
		_ = db.Close()
		owned.finish(true)
		return nil, safeerror.Wrap("initialize migration database driver failed", err)
	}
	owned.finish(false)
	if empty(src.FS) {
		_ = drv.Close()
		return nil, errEmpty
	}
	files, err := iofs.New(src.FS, ".")
	if err != nil {
		_ = drv.Close()
		return nil, safeerror.Wrap("open migration files failed", err)
	}
	m, err := migrate.NewWithInstance("iofs", files, "pgx5", drv)
	if err != nil {
		_ = files.Close()
		_ = drv.Close()
		return nil, safeerror.Wrap("initialize migration engine failed", err)
	}
	m.LockTimeout = cfg.WaitTimeout
	return m, nil
}

// waitTurn serialises runs of one source across processes. golang-migrate takes its own
// advisory lock on the migration connection, but that connection has lock_timeout set, and
// lock_timeout also limits waiting for an advisory lock: a second Job would fail after a few
// seconds instead of waiting. So runs first queue on a separate lock, taken on a connection
// without lock_timeout, and reach golang-migrate's lock only when it is free.
func (r runner) waitTurn(ctx context.Context, cfg Config, src Source) (release func(), err error) {
	pc, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, ErrInvalidDSN
	}
	if pc.RuntimeParams == nil {
		pc.RuntimeParams = map[string]string{}
	}
	pc.RuntimeParams["application_name"] = "migrate-wait"
	pc.RuntimeParams["lock_timeout"] = "0"
	pc.RuntimeParams["statement_timeout"] = "0"

	waitCtx, cancel := context.WithTimeout(ctx, cfg.WaitTimeout)
	defer cancel()
	conn, err := r.connect(waitCtx, pc)
	if err != nil {
		return nil, safeerror.Wrap("connect migration queue failed", err)
	}
	key := "fin-assist/migrate/" + src.Table
	if _, err := conn.Exec(waitCtx, `SELECT pg_advisory_lock(hashtext($1))`, key); err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = conn.Close(closeCtx)
		closeCancel()
		if waitCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("another migration of %s still runs after %s: %w", src.Name, cfg.WaitTimeout, safeerror.Wrap("migration queue timed out", err))
		}
		return nil, safeerror.Wrap("wait for migration lock failed", err)
	}
	return func() {
		bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(bg, `SELECT pg_advisory_unlock(hashtext($1))`, key)
		_ = conn.Close(bg)
	}, nil
}

func open(cfg Config, options ...stdlib.OptionOpenDB) (*sql.DB, error) {
	pc, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, ErrInvalidDSN
	}
	if pc.RuntimeParams == nil {
		pc.RuntimeParams = map[string]string{}
	}
	if pc.ConnectTimeout == 0 && cfg.WaitTimeout > 0 {
		pc.ConnectTimeout = cfg.WaitTimeout
	}
	pc.RuntimeParams["lock_timeout"] = millis(cfg.LockTimeout)
	pc.RuntimeParams["statement_timeout"] = millis(cfg.StatementTimeout)
	pc.RuntimeParams["application_name"] = "migrate"
	return stdlib.OpenDB(*pc, options...), nil
}

func millis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

func empty(fsys fs.FS) bool {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && fileName.MatchString(e.Name()) {
			return false
		}
	}
	return true
}

// Find returns the source with the given name.
func Find(sources []Source, name string) (Source, error) {
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		if s.Name == name {
			return s, nil
		}
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return Source{}, fmt.Errorf("unknown source %q, known: %v", name, names)
}
