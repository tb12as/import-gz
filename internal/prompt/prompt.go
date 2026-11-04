// Package prompt implements the interactive file and database pickers.
package prompt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tb12as/import-gz/internal/importer"
)

// ErrCancelled is returned when the user presses esc or ctrl+c.
var ErrCancelled = errors.New("cancelled")

const maxShown = 10

var (
	r        = lipgloss.NewRenderer(os.Stderr)
	sTitle   = r.NewStyle().Bold(true)
	sMark    = r.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	sSel     = r.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	sDir     = r.NewStyle().Foreground(lipgloss.Color("4"))
	sFaint   = r.NewStyle().Faint(true)
	sErr     = r.NewStyle().Foreground(lipgloss.Color("1"))
	helpText = "up/down select, tab complete, enter confirm, esc cancel"
)

type item struct {
	label  string
	value  string
	detail string
	dir    bool
}

type model struct {
	title   string
	file    bool // file picker; otherwise database picker
	in      textinput.Model
	suggest func(string) []item
	items   []item
	sel     int // -1 means nothing highlighted
	known   []string
	errMsg  string
	result  string
	done    bool
	quit    bool
}

func newModel(title, value, placeholder string, file bool, suggest func(string) []item) model {
	in := textinput.New()
	in.Prompt = sMark.Render("> ")
	in.Placeholder = placeholder
	in.PlaceholderStyle = sFaint
	in.SetValue(value)
	in.CursorEnd()
	in.Focus()

	m := model{title: title, file: file, in: in, suggest: suggest}
	m.refresh()
	return m
}

func (m *model) refresh() {
	m.items = m.suggest(m.in.Value())
	m.sel = -1
	// files: highlight the first match so enter picks it right away
	if m.file && len(m.items) > 0 {
		m.sel = 0
	}
	m.errMsg = ""
}

func (m *model) setInput(v string) {
	m.in.SetValue(v)
	m.in.CursorEnd()
	m.refresh()
}

func (m model) Init() tea.Cmd { return textinput.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		var c tea.Cmd
		m.in, c = m.in.Update(msg)
		return m, c
	}

	n := len(m.items)
	switch k.String() {
	case "ctrl+c", "esc":
		m.quit = true
		return m, tea.Quit
	case "down", "ctrl+n":
		if n > 0 {
			m.sel = (m.sel + 1) % n
		}
		return m, nil
	case "up", "ctrl+p", "shift+tab":
		if n > 0 {
			m.sel = (m.sel - 1 + n) % n
		}
		return m, nil
	case "tab":
		if n == 0 {
			return m, nil
		}
		m.setInput(m.items[max(m.sel, 0)].value)
		return m, nil
	case "enter":
		return m.enter()
	}

	old := m.in.Value()
	var c tea.Cmd
	m.in, c = m.in.Update(msg)
	if m.in.Value() != old {
		m.refresh()
	}
	return m, c
}

func (m model) enter() (tea.Model, tea.Cmd) {
	if m.file {
		// an exact path to a file wins over the highlighted suggestion
		v := expand(m.in.Value())
		if fi, err := os.Stat(v); err == nil && fi.Mode().IsRegular() {
			return m.finish(v)
		}
		if m.sel < 0 {
			m.errMsg = "not a file: " + m.in.Value()
			return m, nil
		}

		it := m.items[m.sel]
		if it.dir {
			m.setInput(it.value)
			return m, nil
		}
		return m.finish(it.value)
	}

	if m.sel >= 0 {
		return m.finish(m.items[m.sel].value)
	}
	v := strings.TrimSpace(m.in.Value())
	if v == "" {
		v = m.in.Placeholder
	}
	if v == "" {
		m.errMsg = "enter a database name"
		return m, nil
	}
	return m.finish(v)
}

func (m model) finish(v string) (tea.Model, tea.Cmd) {
	m.result, m.done = v, true
	return m, tea.Quit
}

func (m model) View() string {
	if m.quit {
		return ""
	}
	if m.done {
		return sTitle.Render(m.title+":") + " " + m.result + "\n"
	}

	var b strings.Builder
	b.WriteString(sMark.Render("? ") + sTitle.Render(m.title) + "  " + sFaint.Render(helpText) + "\n")
	b.WriteString(m.in.View() + "\n")

	if m.errMsg != "" {
		b.WriteString("  " + sErr.Render(m.errMsg) + "\n")
	}

	if !m.file && m.known != nil && m.sel < 0 {
		v := strings.TrimSpace(m.in.Value())
		if v == "" {
			v = m.in.Placeholder
		}
		if v != "" && !slices.Contains(m.known, v) {
			b.WriteString("  " + sFaint.Render("new database, it will be created") + "\n")
		}
	}

	if len(m.items) == 0 {
		if m.file {
			b.WriteString("  " + sFaint.Render("no .sql.gz or .sql files or directories match") + "\n")
		}
		return b.String()
	}

	// scroll window that keeps the highlighted row visible
	start := 0
	if m.sel >= maxShown {
		start = m.sel - maxShown + 1
	}
	end := min(start+maxShown, len(m.items))

	w := 0
	for _, it := range m.items[start:end] {
		w = max(w, len(it.label))
	}

	for i := start; i < end; i++ {
		it := m.items[i]
		label := fmt.Sprintf("%-*s", w, it.label)
		if it.dir {
			label = sDir.Render(label)
		}

		cur := "  "
		if i == m.sel {
			cur = sSel.Render("> ")
			label = sSel.Render(fmt.Sprintf("%-*s", w, it.label))
		}
		b.WriteString("  " + cur + label + "  " + sFaint.Render(it.detail) + "\n")
	}

	if rest := len(m.items) - end; rest > 0 {
		b.WriteString("    " + sFaint.Render(fmt.Sprintf("... %d more", rest)) + "\n")
	}
	return b.String()
}

func run(m model) (string, error) {
	res, err := tea.NewProgram(m, tea.WithOutput(os.Stderr)).Run()
	if err != nil {
		return "", err
	}

	fm := res.(model)
	if !fm.done {
		return "", ErrCancelled
	}
	return fm.result, nil
}

// File asks for a dump file, suggesting dump files and directories as the user types.
func File() (string, error) {
	return run(newModel("Dump file", "", "path to a .sql.gz or .sql file", true, fileSuggester()))
}

// Database asks for a database name, suggesting existing databases.
// known may be nil when the server could not be queried.
func Database(def string, known []string) (string, error) {
	var user []string
	for _, d := range known {
		if !slices.Contains([]string{"information_schema", "mysql", "performance_schema", "sys"}, d) {
			user = append(user, d)
		}
	}

	m := newModel("Database", "", def, false, func(v string) []item {
		v = strings.ToLower(strings.TrimSpace(v))
		var out []item
		for _, d := range user {
			if strings.Contains(strings.ToLower(d), v) {
				out = append(out, item{label: d, value: d, detail: "existing"})
			}
		}
		return out
	})
	m.known = known
	return run(m)
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

func isDump(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".sql.gz") || strings.HasSuffix(n, ".sql")
}

// fileSuggester lists dumps (newest first) and subdirectories matching the typed path.
func fileSuggester() func(string) []item {
	cache := map[string][]os.DirEntry{}

	return func(v string) []item {
		in := expand(v)
		dir, pre := in, ""
		switch {
		case in == "":
			dir = "."
		case strings.HasSuffix(in, "/"):
		default:
			// typing an existing directory name lists its contents right away
			if fi, err := os.Stat(in); err != nil || !fi.IsDir() {
				dir, pre = filepath.Dir(in), filepath.Base(in)
			}
		}

		es, ok := cache[dir]
		if !ok {
			es, _ = os.ReadDir(dir)
			cache[dir] = es
		}

		type dump struct {
			item
			mod time.Time
		}
		var dumps []dump
		var dirs []item
		pre = strings.ToLower(pre)

		for _, e := range es {
			name := e.Name()
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(pre, ".") {
				continue
			}
			if !strings.Contains(strings.ToLower(name), pre) {
				continue
			}

			p := filepath.Join(dir, name)
			fi, err := os.Stat(p) // follows symlinks
			if err != nil {
				continue
			}

			if fi.IsDir() {
				dirs = append(dirs, item{label: name + "/", value: p + "/", detail: "dir", dir: true})
				continue
			}
			if fi.Mode().IsRegular() && isDump(name) {
				d := fmt.Sprintf("%10s  %s", importer.HumanBytes(fi.Size()), fi.ModTime().Format("2006-01-02 15:04"))
				dumps = append(dumps, dump{item{label: name, value: p, detail: d}, fi.ModTime()})
			}
		}

		slices.SortFunc(dumps, func(a, b dump) int { return b.mod.Compare(a.mod) })

		out := make([]item, 0, len(dumps)+len(dirs))
		for _, d := range dumps {
			out = append(out, d.item)
		}
		return append(out, dirs...)
	}
}
