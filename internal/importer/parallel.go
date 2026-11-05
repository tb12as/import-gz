package importer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxHeader = 1 << 20  // header is replayed to every session
	maxDDL    = 64 << 20 // all table definitions are held in memory
)

var (
	verRe = regexp.MustCompile(`^/\*!\d+\s*`)
	// first word after CREATE (and DEFINER/ALGORITHM/SQL SECURITY/OR REPLACE) names a non-table object
	objRe = regexp.MustCompile(`^CREATE(\*/)?(\s+(/\*!\d+\s*)?(DEFINER=\S+|ALGORITHM=\S+|SQL SECURITY \S+|OR REPLACE|\*/))*` +
		`\s+(/\*!\d+\s*)?(VIEW|PROCEDURE|FUNCTION|EVENT)\b`)
)

type rng struct {
	start, end int64
	name       string // table the range loads
}

// plan splits a dump into what each phase of a parallel import runs.
type plan struct {
	header []byte // replayed to every session
	ddl    []byte // all table definitions, run first in one session
	data   []rng  // per-table data (rows, triggers), run concurrently
	tail   int64  // offset of views/routines/events run last, -1 if none
	size   int64  // decompressed dump size
	serial string // non-empty: why the dump cannot be split
	tables int    // CREATE TABLE statements seen
}

type unit struct {
	created bool  // saw CREATE TABLE
	inData  bool  // past the first data statement
	data    int64 // offset of the first data statement
	name    string
}

// tableName returns the table a "CREATE TABLE ..." line creates.
func tableName(line []byte) string {
	s := line[len("CREATE TABLE "):]
	if len(s) >= 14 && bytes.EqualFold(s[:14], []byte("IF NOT EXISTS ")) {
		s = s[14:]
	}

	if len(s) == 0 || s[0] != '`' {
		if i := bytes.IndexAny(s, " (\r\n"); i >= 0 {
			s = s[:i]
		}
		return string(s)
	}

	var b []byte
	for i := 1; i < len(s); i++ {
		if s[i] != '`' {
			b = append(b, s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			b = append(b, '`')
			i++
			continue
		}
		break
	}
	return string(b)
}

func upperPrefix(line []byte) string {
	return strings.ToUpper(string(line[:min(len(line), 160)]))
}

// isTail reports whether a line starts the part that must run after all tables are loaded.
func isTail(u string, cur *unit) bool {
	for _, m := range []string{"-- FINAL VIEW STRUCTURE", "-- DUMPING ROUTINES", "-- DUMPING EVENTS"} {
		if strings.HasPrefix(u, m) {
			return true
		}
	}

	s := verRe.ReplaceAllString(u, "")
	for _, m := range []string{"USE ", "USE`", "CREATE DATABASE", "DROP DATABASE", "CREATE SCHEMA", "DROP SCHEMA",
		"DROP PROCEDURE", "DROP FUNCTION", "DROP EVENT"} {
		if strings.HasPrefix(s, m) {
			return true
		}
	}

	// mysqldump's placeholder view (DROP TABLE v; DROP VIEW v; CREATE VIEW v AS SELECT 1 AS ...) has no
	// CREATE TABLE and no data; it only defines constant columns, so it stays with the table definitions
	placeholder := cur != nil && !cur.created && !cur.inData
	if strings.HasPrefix(s, "DROP VIEW") {
		return !placeholder
	}
	if strings.HasPrefix(s, "CREATE VIEW") && placeholder {
		return false
	}
	return objRe.MatchString(s)
}

func isDataStart(u string) bool {
	for _, m := range []string{"INSERT ", "INSERT\t", "REPLACE ", "LOCK TABLES", "TRUNCATE ", "-- DUMPING DATA"} {
		if strings.HasPrefix(u, m) {
			return true
		}
	}
	return false
}

// headerOK reports whether a header line is safe to replay in every session.
func headerOK(u string) bool {
	t := strings.TrimSpace(u)
	if t == "" || strings.HasPrefix(t, "--") {
		return true
	}

	s := verRe.ReplaceAllString(t, "")
	if strings.HasPrefix(s, "SET ") || strings.HasPrefix(s, "USE ") || strings.HasPrefix(s, "USE`") {
		return true
	}
	return strings.HasPrefix(s, "CREATE DATABASE") && strings.Contains(s, "IF NOT EXISTS")
}

// index reads the dump once and splits it into header, table definitions, per-table data and tail.
// shortcut: splitting is line based and assumes string literals contain no raw newlines (mysqldump and
// Adminer escape them); a dump from a tool that does not would need a real SQL tokenizer.
func index(ctx context.Context, path string, progress io.Writer) (*plan, error) {
	d, err := openDump(path)
	if err != nil {
		return nil, err
	}
	defer d.Close()

	if progress != nil {
		b := &bar{w: progress, n: &d.n.n, size: d.size, start: time.Now(), label: "indexing "}
		defer b.start200ms()()
	}

	p := &plan{tail: -1}
	var cur *unit
	var off int64

	closeUnit := func(end int64) {
		if cur != nil && cur.inData && end > cur.data {
			p.data = append(p.data, rng{cur.data, end, cur.name})
		}
	}

	// dest receives the current line, including the rest of lines longer than the read buffer
	var dest *[]byte
	atStart := true
	limit := func(b *[]byte) int {
		if b == &p.header {
			return maxHeader
		}
		return maxDDL
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line, err := d.body.ReadSlice('\n')
		start := off
		off += int64(len(line))

		if atStart && len(line) > 0 {
			u := upperPrefix(line)
			dest = nil

			switch {
			case strings.HasPrefix(u, "DROP TABLE ") || strings.HasPrefix(u, "CREATE TABLE "):
				if cur == nil || cur.created || cur.inData {
					closeUnit(start)
					cur = &unit{}
				}
				if strings.HasPrefix(u, "CREATE TABLE ") {
					cur.created, cur.name = true, tableName(line)
					p.tables++
				}
				dest = &p.ddl
			case isTail(u, cur) && cur != nil:
				closeUnit(start)
				p.tail = start
			case cur == nil:
				if !headerOK(u) {
					p.serial = "the dump header has statements that cannot be repeated per session"
					return p, nil
				}
				dest = &p.header
			case !cur.inData && isDataStart(u):
				cur.inData, cur.data = true, start
			case !cur.inData:
				dest = &p.ddl
			}

			if p.tail >= 0 {
				break
			}
		}

		if dest != nil {
			if len(*dest)+len(line) > limit(dest) {
				p.serial = "the dump header or table definitions are too large to hold in memory"
				return p, nil
			}
			*dest = append(*dest, line...)
		}

		atStart = !errors.Is(err, bufio.ErrBufferFull)
		if err == io.EOF {
			break
		}
		if err != nil && atStart {
			return nil, err
		}
	}

	if p.tail < 0 {
		closeUnit(off)
	}

	// the tail still has to be read to learn the total size
	if p.tail >= 0 {
		n, err := io.Copy(io.Discard, d.body)
		if err != nil {
			return nil, err
		}
		off += n
	}
	p.size = off

	if cur == nil {
		p.serial = "no tables found"
	}
	return p, nil
}

// feed runs one mysql session: prefix and header, then every rng handed out by next, then the suffix.
func (o Options) feed(ctx context.Context, head string, next func() (rng, bool), stderr io.Writer,
	done *atomic.Int64) error {
	d, err := openDump(o.File)
	if err != nil {
		return err
	}
	defer d.Close()

	cmd := exec.CommandContext(ctx, o.Bin, append(slices.Clone(o.Args), "--database="+o.DB)...)
	cmd.Stdout, cmd.Stderr = o.Stdout, stderr
	cmd.WaitDelay = 2 * time.Second

	w, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	werr := func() error {
		if _, err := io.WriteString(w, head); err != nil {
			return err
		}

		var pos int64
		for {
			s, ok := next()
			if !ok {
				break
			}

			// spans are handed out in stream order, so skipping only ever moves forward
			if _, err := d.body.Discard(int(s.start - pos)); err != nil {
				return err
			}
			n, err := io.CopyN(w, d.body, s.end-s.start)
			done.Add(n)
			pos = s.start + n
			if err != nil {
				return err
			}
		}

		_, err := io.WriteString(w, suffix)
		return err
	}()
	w.Close()

	// mysql's own exit status explains a broken pipe better than the write error
	if err := cmd.Wait(); err != nil {
		return err
	}
	return werr
}

// runParallel imports a split dump: table definitions, then per-table data in o.Parallel sessions,
// then the tail. shortcut: the unit of work is a table, so one huge table still loads in one session.
func runParallel(ctx context.Context, o Options, p *plan, stderr io.Writer, done *atomic.Int64,
	cur *atomic.Pointer[string]) error {
	set := func(s string) { cur.Store(&s) }
	head := sessionPrefix(o.TimeZone) + string(rewriteHeader(p.header, o.TimeZone))

	set("creating tables")
	ddl := io.MultiReader(strings.NewReader(head), bytes.NewReader(p.ddl), strings.NewReader(suffix))
	if err := o.mysql(ctx, ddl, stderr, "--database="+o.DB); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	i := 0
	next := func() (rng, bool) {
		mu.Lock()
		defer mu.Unlock()

		if i == len(p.data) {
			return rng{}, false
		}
		i++
		set(fmt.Sprintf("%d/%d %s", i, len(p.data), p.data[i-1].name))
		return p.data[i-1], true
	}

	var first error
	var once sync.Once
	var wg sync.WaitGroup
	for range min(o.Parallel, len(p.data)) {
		wg.Go(func() {
			if err := o.feed(wctx, head, next, stderr, done); err != nil {
				// keep the error that caused the cancel, not the "killed" errors that follow it
				once.Do(func() { first = err })
				cancel()
			}
		})
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}
	if first != nil {
		return first
	}

	if p.tail < 0 {
		return nil
	}
	set("views and routines")
	tail := []rng{{p.tail, p.size, ""}}
	return o.feed(ctx, head, func() (rng, bool) {
		if len(tail) == 0 {
			return rng{}, false
		}
		s := tail[0]
		tail = tail[1:]
		return s, true
	}, stderr, done)
}

// total is the number of bytes the data and tail phases send, for the progress bar.
func (p *plan) total() int64 {
	var n int64
	for _, s := range p.data {
		n += s.end - s.start
	}
	if p.tail >= 0 {
		n += p.size - p.tail
	}
	return n
}
