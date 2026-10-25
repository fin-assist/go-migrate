package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func TestUnitUpAndDownRefuseAppliedVersionWithoutFile(t *testing.T) {
	cfg := DefaultConfig("postgres://localhost/db")
	for _, operation := range []string{"up", "down"} {
		t.Run(operation, func(t *testing.T) {
			q := &fakeQueue{}
			steps := 0
			// Version 7 is recorded in the version table, the source has only 1..3. The
			// engine would report os.ErrNotExist, the same error as "no next file".
			m := &fakeSession{v: 7, steps: func(int) error { steps++; return os.ErrNotExist }}
			r := fakeRunner(q, m)
			var err error
			if operation == "up" {
				err = r.up(context.Background(), cfg, []Source{source()})
			} else {
				err = r.down(context.Background(), cfg, source(), 1)
			}
			if err == nil || !strings.Contains(err.Error(), "applied version 7 of source service has no migration file") ||
				!strings.HasSuffix(err.Error(), "force an existing version, or force 0 when nothing is applied") {
				t.Fatalf("false success: %v", err)
			}
			if steps != 0 || m.closed != 1 || q.closed != 1 {
				t.Fatalf("steps=%d session closes=%d queue closes=%d", steps, m.closed, q.closed)
			}
		})
	}
}

func TestUnitAppliedVersionCheckLeavesDirtyAndUnreadableToTheEngine(t *testing.T) {
	// A dirty unknown version is refused by the engine with ErrDirty, as before.
	dirty := &fakeSession{v: 7, dirty: true}
	if err := appliedVersionKnown(dirty, source()); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("version table unreadable")
	if err := appliedVersionKnown(&fakeSession{versionErr: sentinel}, source()); !errors.Is(err, sentinel) || strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("version error must stay programmatic only: %v", err)
	}
	if err := appliedVersionKnown(&fakeSession{v: 3}, Source{Name: "broken", FS: brokenFS{}}); err == nil {
		t.Fatal("unreadable source accepted")
	}
	for _, v := range []uint{0, 1, 3} {
		if err := appliedVersionKnown(&fakeSession{v: v}, source()); err != nil {
			t.Fatalf("version %d: %v", v, err)
		}
	}
}

func TestUnitForceAcceptsOnlyVersionsOfTheSource(t *testing.T) {
	cfg := DefaultConfig("postgres://localhost/db")
	withZero := Source{Name: "zero", Table: "zero_versions", FS: fstest.MapFS{
		"0_init.up.sql": {Data: []byte("SELECT 1")}, "0_init.down.sql": {Data: []byte("SELECT 1")},
	}}
	for _, test := range []struct {
		name    string
		src     Source
		version int
		want    int
	}{
		{"existing", source(), 2, 2},
		{"nothing-applied", source(), -1, -1},
		{"zero-means-nothing-applied", source(), 0, -1},
		{"zero-file", withZero, 0, 0},
		{"zero-file-nothing-applied", withZero, -1, -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := 0
			m := &fakeSession{}
			r := fakeRunner(&fakeQueue{}, m)
			r.open = func(Config, Source) (migrationSession, error) {
				return forceRecorder{fakeSession: m, got: &got}, nil
			}
			if err := r.force(context.Background(), cfg, test.src, test.version); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("engine version %d, want %d", got, test.want)
			}
		})
	}
	for _, version := range []int{4, 40, -2} {
		r := runner{connect: nil, open: nil} // refused before any database work
		err := r.force(context.Background(), cfg, source(), version)
		if err == nil || !strings.Contains(err.Error(), "has no migration file") {
			t.Fatalf("version %d accepted: %v", version, err)
		}
	}
}

type forceRecorder struct {
	*fakeSession
	got *int
}

func (f forceRecorder) Force(v int) error { *f.got = v; return f.fakeSession.Force(v) }

func TestUnitValidateRejectsSameVersionWithDifferentWidths(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"up":   {"1_a.up.sql": {}, "01_a.up.sql": {}, "1_a.down.sql": {}},
		"down": {"1_a.up.sql": {}, "1_a.down.sql": {}, "001_a.down.sql": {}},
	} {
		t.Run(name, func(t *testing.T) {
			err := Validate(Source{Name: "consumer", FS: files})
			if err == nil || !strings.Contains(err.Error(), "several up or down files") {
				t.Fatalf("duplicate version accepted: %v", err)
			}
		})
	}
	overflow := fstest.MapFS{"18446744073709551616_a.up.sql": {}, "18446744073709551616_a.down.sql": {}}
	if err := Validate(Source{Name: "consumer", FS: overflow}); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("overflowing version accepted: %v", err)
	}
	if err := Validate(Source{Name: "consumer", FS: fstest.MapFS{"1_a.up.sql": {}, "01_a.down.sql": {}}}); err != nil {
		t.Fatalf("one up and one down of the same version in different widths: %v", err)
	}
}

func TestUnitEmptySourceWithRecordedVersion(t *testing.T) {
	cfg := DefaultConfig("postgres://localhost/db")
	recorded := emptyApplied{source: "service", version: 5}
	open := func(Config, Source) (migrationSession, error) { return nil, recorded }
	r := fakeRunner(&fakeQueue{}, &fakeSession{})
	r.open = open
	empty := Source{Name: "service", Table: "schema_migrations", FS: fstest.MapFS{}}
	for name, err := range map[string]error{
		"up":   r.up(context.Background(), cfg, []Source{empty}),
		"down": r.down(context.Background(), cfg, empty, 1),
	} {
		if err == nil || !strings.Contains(err.Error(), "applied version 5 of source service has no migration file") {
			t.Fatalf("%s: recorded version of an empty source ignored: %v", name, err)
		}
	}
	recorded.dirty = true
	st, err := r.versions(context.Background(), cfg, []Source{empty})
	if err != nil || len(st) != 1 || st[0].Version != 5 || !st[0].Dirty {
		t.Fatalf("versions %v %v", st, err)
	}
	r.open = func(Config, Source) (migrationSession, error) { return nil, errEmpty }
	if err := r.up(context.Background(), cfg, []Source{empty}); err != nil {
		t.Fatalf("empty source without a version must be skipped: %v", err)
	}
}
