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
	"github.com/golang-migrate/migrate/v4/database"
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
// version has exactly one up and one down file, and versions are unique. Versions are
// compared numerically, so 1_a.up.sql and 01_a.up.sql are the same version and rejected
// together. A down file is mandatory (FIN-115): an irreversible step still has one, which
// fails with an explanation.
func Validate(src Source) error {
	_, err := sourceVersions(src)
	return err
}

// sourceVersions validates the files of src and returns its migration versions.
func sourceVersions(src Source) (map[uint64]bool, error) {
	if src.FS == nil {
		return nil, fmt.Errorf("source %s: files are required", src.Name)
	}
	entries, err := fs.ReadDir(src.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", src.Name, err)
	}
	type pair struct {
		name     string
		up, down int
	}
	versions := map[uint64]*pair{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "embed.go" || !isSQL(e.Name()) {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("source %s: %q does not match NNN_name.up.sql / NNN_name.down.sql", src.Name, e.Name())
		}
		v, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("source %s: %q: version is out of range", src.Name, e.Name())
		}
		p := versions[v]
		if p == nil {
			p = &pair{name: m[2]}
			versions[v] = p
		} else if p.name != m[2] {
			return nil, fmt.Errorf("source %s: version %d is used by %q and %q", src.Name, v, p.name, m[2])
		}
		switch m[3] {
		case "up":
			p.up++
		case "down":
			p.down++
		}
	}
	known := make(map[uint64]bool, len(versions))
	for v, p := range versions {
		if p.up == 0 {
			return nil, fmt.Errorf("source %s: version %d (%s) has no up file", src.Name, v, p.name)
		}
		if p.down == 0 {
			return nil, fmt.Errorf("source %s: version %d (%s) has no down file", src.Name, v, p.name)
		}
		if p.up > 1 || p.down > 1 {
			return nil, fmt.Errorf("source %s: version %d (%s) has several up or down files", src.Name, v, p.name)
		}
		known[v] = true
	}
	return known, nil
}

// appliedVersionKnown fails when the clean version recorded in the version table has no
// file in the source (a renamed or deleted applied file, a forced version without a file).
// golang-migrate reports that case with os.ErrNotExist, the same error as "no next file",
// so stepping without this check would end as a false "up to date" (FIN-278).
// A dirty version is left to the engine, which refuses it with ErrDirty.
func appliedVersionKnown(m migrationSession, src Source) error {
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) || (err == nil && dirty) {
		return nil
	}
	if err != nil {
		return safeerror.Wrap("read migration version failed", err)
	}
	known, err := sourceVersions(src)
	if err != nil {
		return err
	}
	if !known[uint64(v)] {
		return appliedWithoutFile(v, src.Name)
	}
	return nil
}

func appliedWithoutFile(v uint, name string) error {
	return fmt.Errorf("applied version %d of source %s has no migration file: check the schema, restore the file, force an existing version, or force 0 when nothing is applied", v, name)
}

// emptyApplied: the source has no migration files, but its version table records a
// version (an older image of a source whose first migration a newer image applied).
// Up and Down refuse it like any applied version without a file; Versions reports it.
// noVersion marks a dirty row without a version: golang-migrate records (-1, dirty)
// when a rollback of the first migration fails.
type emptyApplied struct {
	source    string
	version   uint
	dirty     bool
	noVersion bool
}

// Error points to a build that has the source's files: Force cannot work in this one.
// A dirty version refuses Down there too (ErrDirty), so it needs Force in that build.
func (e emptyApplied) Error() string {
	if e.noVersion {
		return fmt.Sprintf("source %s is dirty with no applied version (a failed rollback of its first migration): this build has no files of the source; check the schema and force a version with a build that has the file, or skip this migration run", e.source)
	}
	prefix := fmt.Sprintf("applied version %d of source %s has no migration file: this build has no files of the source; ", e.version, e.source)
	if e.dirty {
		return prefix + "the version is dirty: check the schema and force a version with a build that has the file, or skip this migration run"
	}
	return prefix + "check the schema and run Down with a build that has the file, or skip this migration run"
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
			if err := appliedVersionKnown(m, s); err != nil {
				return err
			}
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
		var recorded emptyApplied
		if errors.As(err, &recorded) {
			st.Version, st.Dirty, err = recorded.version, recorded.dirty, nil
		}
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
		if err := appliedVersionKnown(m, src); err != nil {
			return err
		}
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
// Only call it after the actual schema has been checked by hand. version is a version
// that has a file in the source, or 0 / -1 for "nothing applied" (0 means a real version
// only when the source has a version 0 file, matching Versions, which reports 0 for an
// empty table). Any other version is refused before connecting: the engine would accept
// it and every later Up would find no next file.
func Force(ctx context.Context, cfg Config, src Source, version int) error {
	return production().force(ctx, cfg, src, version)
}

func (r runner) force(ctx context.Context, cfg Config, src Source, version int) error {
	if err := validateAll([]Source{src}); err != nil {
		return err
	}
	target, err := forceTarget(src, version)
	if err != nil {
		return err
	}
	return r.withMigrate(ctx, cfg, src, func(m migrationSession) error {
		if err := m.Force(target); err != nil {
			return safeerror.Wrap("force migration version failed", err)
		}
		cfg.logger().WarnContext(ctx, "migration version forced", "source", src.Name, "version", version)
		return nil
	})
}

// forceTarget maps the operator's version to the engine's: -1 (and 0 without a version 0
// file) is the nil version, otherwise the version must exist in the source.
func forceTarget(src Source, version int) (int, error) {
	known, err := sourceVersions(src)
	if err != nil {
		return 0, err
	}
	switch {
	case version == -1, version == 0 && !known[0]:
		return -1, nil
	case version >= 0 && known[uint64(version)]:
		return version, nil
	}
	return 0, fmt.Errorf("source %s: version %d has no migration file; force an existing version, or 0 when nothing is applied", src.Name, version)
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
		var recorded emptyApplied
		if (errors.Is(err, errEmpty) || errors.As(err, &recorded)) && ctx.Err() != nil {
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
		v, dirty, err := drv.Version()
		_ = drv.Close()
		if err != nil {
			return nil, safeerror.Wrap("read migration version failed", err)
		}
		if v == database.NilVersion && dirty {
			return nil, emptyApplied{source: src.Name, dirty: true, noVersion: true}
		}
		if v != database.NilVersion {
			return nil, emptyApplied{source: src.Name, version: uint(v), dirty: dirty}
		}
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
	return engineSession{Migrate: m, readVersion: drv.Version}, nil
}

// engineSession reads the version from the driver instead of Migrate.Version, which maps
// the nil version to ErrNilVersion and drops the dirty flag. golang-migrate records
// (-1, dirty) when the rollback of the first migration fails; Versions must report it as
// dirty 0, not as a clean "nothing applied" (FIN-314).
type engineSession struct {
	*migrate.Migrate
	readVersion func() (int, bool, error)
}

// Version returns the applied version, ErrNilVersion for a clean empty table and
// (0, true, nil) for a dirty row without a version.
func (s engineSession) Version() (uint, bool, error) {
	return driverVersion(s.readVersion)
}

func driverVersion(read func() (int, bool, error)) (uint, bool, error) {
	v, dirty, err := read()
	switch {
	case err != nil:
		return 0, false, err
	case v == database.NilVersion && dirty:
		return 0, true, nil
	case v < 0:
		return 0, false, migrate.ErrNilVersion
	}
	return uint(v), dirty, nil
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
