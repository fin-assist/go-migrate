package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	engine "github.com/golang-migrate/migrate/v4"

	"github.com/fin-assist/go-migrate/internal/safeerror"
)

// IrreversibleMarker declares a down file that deliberately refuses rollback.
const IrreversibleMarker = "-- irreversible"

// CheckUpDownUp checks each migration against a caller-owned empty database.
// It applies one version, rolls it back, verifies the previous version, then applies it
// again. Irreversible down files are not executed. The caller owns database cleanup.
func CheckUpDownUp(ctx context.Context, cfg Config, src Source) error {
	return production().check(ctx, cfg, src)
}
func (r runner) check(ctx context.Context, cfg Config, src Source) error {
	if err := validateAll([]Source{src}); err != nil {
		return err
	}
	err := r.withMigrate(ctx, cfg, src, func(m migrationSession) error {
		for {
			before, err := cleanVersion(m)
			if err != nil {
				return err
			}
			err = step(ctx, m, 1)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, engine.ErrNoChange) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: up from %d: %w", src.Name, before, err)
			}
			v, err := cleanVersion(m)
			if err != nil {
				return err
			}
			irreversible, err := irreversible(src, v)
			if err != nil {
				return err
			}
			if irreversible {
				continue
			}
			if err := step(ctx, m, -1); err != nil {
				return fmt.Errorf("%s: down from %d: %w", src.Name, v, err)
			}
			got, err := cleanVersion(m)
			if err != nil {
				return err
			}
			if got != before {
				return fmt.Errorf("%s: down from %d left version %d, want %d", src.Name, v, got, before)
			}
			if err := step(ctx, m, 1); err != nil {
				return fmt.Errorf("%s: up again to %d: %w", src.Name, v, err)
			}
		}
	})
	if errors.Is(err, errEmpty) {
		return nil
	}
	return err
}
func cleanVersion(m migrationSession) (uint, error) {
	v, dirty, err := m.Version()
	if errors.Is(err, engine.ErrNilVersion) {
		return 0, nil
	}
	if err != nil {
		return 0, safeerror.Wrap("read migration version failed", err)
	}
	if dirty {
		return 0, fmt.Errorf("dirty at version %d", v)
	}
	return v, nil
}
func irreversible(src Source, v uint) (bool, error) {
	entries, err := fs.ReadDir(src.FS, ".")
	if err != nil {
		return false, err
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		parts := fileName.FindStringSubmatch(entry.Name())
		if parts == nil || parts[3] != "down" {
			continue
		}
		version, err := strconv.ParseUint(parts[1], 10, 64)
		if err == nil && version == uint64(v) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) != 1 {
		return false, fmt.Errorf("%s: down file of %d: %v", src.Name, v, matches)
	}
	b, err := fs.ReadFile(src.FS, matches[0])
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.TrimSpace(string(b)), IrreversibleMarker), nil
}
