package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	engine "github.com/golang-migrate/migrate/v4"
)

func TestUnitCheckWalkAndFailures(t *testing.T) {
	sentinel := errors.New("engine failure")
	for _, mode := range []string{"success", "irreversible", "initial-dirty", "initial-version", "up", "applied-dirty", "down", "down-version", "down-noop", "reapply", "missing-down", "read-down", "no-change", "empty"} {
		t.Run(mode, func(t *testing.T) {
			src := source()
			q := &fakeQueue{}
			m := &fakeSession{}
			r := fakeRunner(q, m)
			calls := 0
			m.steps = func(n int) error {
				calls++
				if calls == 1 {
					switch mode {
					case "up":
						return sentinel
					case "no-change":
						return engine.ErrNoChange
					}
				}
				if n < 0 {
					if mode == "down" {
						return sentinel
					}
					if mode != "down-noop" {
						m.v--
					}
					if mode == "down-version" {
						m.versionErr = sentinel
					}
					return nil
				}
				if calls == 3 && mode == "reapply" {
					return sentinel
				}
				if m.v == 1 {
					return engine.ErrNoChange
				}
				m.v++
				if mode == "applied-dirty" {
					m.dirty = true
				}
				return nil
			}
			switch mode {
			case "initial-dirty":
				m.dirty = true
			case "initial-version":
				m.versionErr = sentinel
			case "irreversible":
				src.FS.(fstest.MapFS)["000001_a.down.sql"].Data = []byte(" -- irreversible\nSELECT 1")
			case "missing-down":
				src.FS = fstest.MapFS{"002_a.up.sql": {}, "002_a.down.sql": {}}
			case "read-down":
				src.FS = unreadableDown{src.FS}
			case "empty":
				r.open = func(Config, Source) (migrationSession, error) { return nil, errEmpty }
			}
			err := r.check(context.Background(), DefaultConfig("postgres://localhost/db"), src)
			if mode == "success" || mode == "irreversible" || mode == "no-change" || mode == "empty" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "success" && (m.v != 1 || calls != 4) {
				t.Fatalf("version=%d steps=%d", m.v, calls)
			}
			if mode == "down-noop" && !strings.Contains(err.Error(), "want 0") {
				t.Fatal(err)
			}
			if q.closed != 1 {
				t.Fatal("queue leaked")
			}
		})
	}
}

type unreadableDown struct{ fs.FS }

func (f unreadableDown) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".down.sql") {
		return nil, fs.ErrPermission
	}
	return f.FS.Open(name)
}

func TestUnitIrreversibleMatchesNumericVersionWithoutFixedWidth(t *testing.T) {
	for _, prefix := range []string{"1", "001", "000001", "00000001"} {
		for _, marked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marked=%t", prefix, marked), func(t *testing.T) {
				body := "DROP TABLE item;"
				if marked {
					body = "  -- irreversible\n" + body
				}
				src := Source{Name: "consumer", FS: fstest.MapFS{
					prefix + "_item.up.sql": {}, prefix + "_item.down.sql": {Data: []byte(body)},
					"002_other.down.sql": {}, "notes.txt": {}, "1_bad-name.down.sql": {},
					"18446744073709551616_overflow.down.sql": {},
				}}
				got, err := irreversible(src, 1)
				if err != nil || got != marked {
					t.Fatalf("marker=%t err=%v", got, err)
				}
			})
		}
	}
}

func TestUnitIrreversibleRejectsMissingAndAmbiguousNumericVersions(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"missing":          {"002_item.down.sql": {}},
		"ambiguous-widths": {"001_item.down.sql": {}, "000001_item.down.sql": {}},
		"ambiguous-names":  {"001_item.down.sql": {}, "000001_other.down.sql": {}},
		"directory":        {"001_item.down.sql": {Mode: fs.ModeDir}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := irreversible(Source{Name: "consumer", FS: files}, 1); err == nil {
				t.Fatal("missing or ambiguous down file accepted")
			}
		})
	}
}

func TestUnitIrreversiblePropagatesDirectoryReadFailure(t *testing.T) {
	_, err := irreversible(Source{Name: "consumer", FS: unreadableRoot{}}, 1)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("directory failure lost: %v", err)
	}
}

type unreadableRoot struct{}

func (unreadableRoot) Open(string) (fs.File, error) { return nil, fs.ErrPermission }
