package archiver

import (
	"errors"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
)

func TestFixedReadsAnArchiveWithoutTakingIt(t *testing.T) {
	root := t.TempDir()
	if err := archive.Init(root); err != nil {
		t.Fatal(err)
	}
	w, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Add(archive.User, archive.Entry{Symbol: "VTI"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	f, err := OpenFixed(root)
	if err != nil {
		t.Fatalf("a fixed reader couldn't open an archive a writer holds: %v", err)
	}
	defer f.Close()
	err = f.Read(func(a *archive.Archive) error {
		if !a.ReadOnly() {
			t.Error("the fixed archive isn't read-only")
		}
		_, err := a.Lookup("VTI")
		return err
	})
	if err != nil {
		t.Errorf("reading the writer's symbol: %v", err)
	}
	st := f.Status(false)
	if st.State != StateOpen || st.Path != root {
		t.Errorf("status = %+v, want open at %s", st, root)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.Status(false).Stats == nil {
		if time.Now().After(deadline) {
			t.Fatal("the background count never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.Status(false).Stats.Active != 1 {
		t.Errorf("stats = %+v, want the one active symbol", f.Status(false).Stats)
	}
	if _, err := OpenFixed(t.TempDir()); !errors.Is(err, archive.ErrUnavailable) {
		t.Errorf("a folder that isn't an archive gave %v, want ErrUnavailable", err)
	}
}
