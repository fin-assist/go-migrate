package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	engine "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

// TestUnitDriverVersionKeepsDirtyNilVersion reproduces FIN-314: the engine's Version maps the
// (-1, dirty) row of a failed first rollback to ErrNilVersion and loses the dirty flag.
func TestUnitDriverVersionKeepsDirtyNilVersion(t *testing.T) {
	sentinel := errors.New("version table unreadable")
	for _, tc := range []struct {
		name      string
		v         int
		dirty     bool
		readErr   error
		want      uint
		wantDirty bool
		wantErr   error
	}{
		{name: "dirty nil version", v: database.NilVersion, dirty: true, wantDirty: true},
		{name: "clean nil version", v: database.NilVersion, wantErr: engine.ErrNilVersion},
		{name: "clean version", v: 3, want: 3},
		{name: "dirty version", v: 2, dirty: true, want: 2, wantDirty: true},
		{name: "version zero", v: 0, want: 0},
		{name: "read failure", v: 5, dirty: true, readErr: sentinel, wantErr: sentinel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, dirty, err := driverVersion(func() (int, bool, error) { return tc.v, tc.dirty, tc.readErr })
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("error %v, want %v", err, tc.wantErr)
			}
			if v != tc.want || dirty != tc.wantDirty {
				t.Fatalf("got (%d, %v), want (%d, %v)", v, dirty, tc.want, tc.wantDirty)
			}
		})
	}
	s := engineSession{readVersion: func() (int, bool, error) { return database.NilVersion, true, nil }}
	if v, dirty, err := s.Version(); v != 0 || !dirty || err != nil {
		t.Fatalf("session version (%d, %v, %v), want dirty 0", v, dirty, err)
	}
}

// TestUnitDirtyNilVersionIsReportedAndLeftToTheEngine: with the session reporting dirty 0,
// Versions shows it, the applied-file check does not hide the engine's ErrDirty and the
// up/down/up walk refuses to start.
func TestUnitDirtyNilVersionIsReportedAndLeftToTheEngine(t *testing.T) {
	cfg := DefaultConfig("postgres://localhost/db")
	m := &fakeSession{v: 0, dirty: true}
	r := fakeRunner(&fakeQueue{}, m)
	st, err := r.versions(context.Background(), cfg, []Source{source()})
	if err != nil || len(st) != 1 || st[0].Version != 0 || !st[0].Dirty {
		t.Fatalf("versions %v %v, want dirty 0", st, err)
	}
	if err := appliedVersionKnown(m, source()); err != nil {
		t.Fatalf("dirty nil version must be refused by the engine, not as a missing file: %v", err)
	}
	if _, err := cleanVersion(m); err == nil || !strings.Contains(err.Error(), "dirty at version 0") {
		t.Fatalf("clean version of dirty 0: %v", err)
	}
	if err := r.check(context.Background(), cfg, source()); err == nil || !strings.Contains(err.Error(), "dirty at version 0") {
		t.Fatalf("walk started on a dirty source: %v", err)
	}
}
