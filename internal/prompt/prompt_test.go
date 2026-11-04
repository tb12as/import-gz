package prompt

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func fixture(t *testing.T) string {
	t.Helper()

	d := t.TempDir()
	now := time.Now()
	for i, n := range []string{"old.sql.gz", "new.sql", "notes.txt", ".hidden.sql.gz"} {
		p := filepath.Join(d, n)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, now, now.Add(time.Duration(i)*time.Hour))
	}
	os.Mkdir(filepath.Join(d, "sub"), 0o755)
	return d
}

func labels(its []item) []string {
	var out []string
	for _, it := range its {
		out = append(out, it.label)
	}
	return out
}

func TestFileSuggester(t *testing.T) {
	d := fixture(t)
	s := fileSuggester()

	cases := map[string][]string{
		d + "/":     {"new.sql", "old.sql.gz", "sub/"}, // newest dump first, dirs last, no txt/hidden
		d:           {"new.sql", "old.sql.gz", "sub/"}, // existing dir without slash lists its contents
		d + "/OL":   {"old.sql.gz"},
		d + "/.h":   {".hidden.sql.gz"},
		d + "/zzz":  nil,
		d + "/su":   {"sub/"},
		d + "/sub/": nil,
	}
	for in, want := range cases {
		got := labels(s(in))
		if len(got) != len(want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: got %v, want %v", in, got, want)
				break
			}
		}
	}
}

func key(m model, k tea.KeyType) model {
	r, _ := m.Update(tea.KeyMsg{Type: k})
	return r.(model)
}

func TestFileKeys(t *testing.T) {
	d := fixture(t)

	// enter picks the highlighted (newest) dump
	m := key(newModel("f", d+"/", "", true, fileSuggester()), tea.KeyEnter)
	if !m.done || m.result != filepath.Join(d, "new.sql") {
		t.Errorf("enter: done=%v result=%q", m.done, m.result)
	}

	// down then enter picks the second one
	m = key(key(newModel("f", d+"/", "", true, fileSuggester()), tea.KeyDown), tea.KeyEnter)
	if m.result != filepath.Join(d, "old.sql.gz") {
		t.Errorf("down+enter: result=%q", m.result)
	}

	// enter on a directory navigates into it instead of finishing
	m = key(newModel("f", d+"/su", "", true, fileSuggester()), tea.KeyEnter)
	if m.done || m.in.Value() != filepath.Join(d, "sub")+"/" {
		t.Errorf("enter on dir: done=%v value=%q", m.done, m.in.Value())
	}

	// tab completes the highlighted entry into the input
	m = key(newModel("f", d+"/ol", "", true, fileSuggester()), tea.KeyTab)
	if m.done || m.in.Value() != filepath.Join(d, "old.sql.gz") {
		t.Errorf("tab: value=%q", m.in.Value())
	}

	// esc cancels
	if m = key(newModel("f", d, "", true, fileSuggester()), tea.KeyEsc); !m.quit || m.done {
		t.Error("esc should cancel")
	}
}

func TestDatabaseKeys(t *testing.T) {
	sug := func(string) []item { return []item{{label: "shop", value: "shop"}} }

	// enter with nothing highlighted uses the default
	m := key(newModel("db", "", "myapp", false, sug), tea.KeyEnter)
	if m.result != "myapp" {
		t.Errorf("default: %q", m.result)
	}

	// down selects an existing database
	m = key(key(newModel("db", "", "myapp", false, sug), tea.KeyDown), tea.KeyEnter)
	if m.result != "shop" {
		t.Errorf("select: %q", m.result)
	}

	// no default and empty input does not finish
	if m = key(newModel("db", "", "", false, sug), tea.KeyEnter); m.done {
		t.Error("empty name accepted")
	}
}
