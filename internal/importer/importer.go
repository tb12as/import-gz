// Package importer streams a (gzipped) sql dump into the mysql client.
package importer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// autocommit=0 avoids one commit (and fsync) per insert; mysql recommends it for bulk loads
const (
	prefix = "SET FOREIGN_KEY_CHECKS=0;\nSET AUTOCOMMIT=0;\n"
	suffix = "\nCOMMIT;\nSET FOREIGN_KEY_CHECKS=1;\n"
)

// ErrNoClient is returned when the mysql client binary cannot be found.
var ErrNoClient = errors.New("mysql client not found")

type Options struct {
	File     string
	DB       string
	Bin      string   // mysql client binary
	Args     []string // extra args passed to every mysql call
	Create   bool     // create database if missing
	Fresh    bool     // drop and recreate database
	TimeZone string   // if set, session time_zone for the import (overrides the dump header)
	Progress bool     // draw progress bar on Stderr
	Parallel int      // >= 2: load tables in this many concurrent sessions
	Stdout   io.Writer
	Stderr   io.Writer
}

type Result struct {
	Bytes   int64 // bytes read from the dump file
	Tables  int   // CREATE TABLE statements sent
	Elapsed time.Duration
}

// dump is an open dump file; body yields the decompressed sql
type dump struct {
	f    *os.File
	gz   *gzip.Reader
	n    *counter
	size int64
	mod  time.Time
	body *bufio.Reader
}

func (d *dump) Close() error {
	if d.gz != nil {
		d.gz.Close()
	}
	return d.f.Close()
}

type stream struct {
	io.Reader
	*dump
}

// openStream returns prefix + dump body + suffix; a non-empty tz overrides the dump's time_zone.
func openStream(path, tz string) (*stream, error) {
	if tz != "" && !ValidTZ(tz) {
		return nil, fmt.Errorf("invalid time zone %q", tz)
	}

	d, err := openDump(path)
	if err != nil {
		return nil, err
	}

	var body io.Reader = d.body
	if tz != "" {
		body = io.MultiReader(bytes.NewReader(rewriteHeader(header(d.body), tz)), d.body)
	}

	return &stream{io.MultiReader(strings.NewReader(sessionPrefix(tz)), body, strings.NewReader(suffix)), d}, nil
}

const headMax = 256 // enough for CREATE TABLE plus a 64-char name, even fully escaped

// tableScan passes a stream through and records each table it creates.
type tableScan struct {
	r    io.Reader
	cur  *atomic.Pointer[string]
	n    int
	head []byte // start of the current line, up to headMax bytes
	done bool   // head has been checked for this line
}

func (t *tableScan) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)

	for b := p[:n]; len(b) > 0; {
		seg := b
		i := bytes.IndexByte(b, '\n')
		if i >= 0 {
			seg, b = b[:i], b[i+1:]
		} else {
			b = nil
		}

		if !t.done {
			t.head = append(t.head, seg[:min(len(seg), headMax-len(t.head))]...)
			if i >= 0 || len(t.head) == headMax {
				t.check()
				t.done = true
			}
		}
		if i >= 0 {
			t.head, t.done = t.head[:0], false
		}
	}
	return n, err
}

func (t *tableScan) check() {
	const m = "CREATE TABLE "
	if len(t.head) < len(m) || !bytes.EqualFold(t.head[:len(m)], []byte(m)) {
		return
	}

	s := tableName(t.head)
	t.n++
	t.cur.Store(&s)
}

// sessionPrefix is sent before the dump in every mysql session.
func sessionPrefix(tz string) string {
	if tz == "" {
		return prefix
	}
	return prefix + "SET time_zone = '" + tz + "';\n"
}

// rewriteHeader points every time_zone assignment in a dump header at tz.
func rewriteHeader(h []byte, tz string) []byte {
	if tz == "" {
		return h
	}
	return tzRe.ReplaceAll(h, []byte("${1}'"+tz+"'"))
}

func openDump(path string) (*dump, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	c := &counter{r: f}
	br := bufio.NewReaderSize(c, 64<<10)
	d := &dump{f: f, n: c, size: st.Size(), mod: st.ModTime(), body: br}

	// detect gzip by magic bytes so plain .sql files work too
	if m, _ := br.Peek(2); len(m) == 2 && m[0] == 0x1f && m[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("read gzip header: %w", err)
		}
		d.gz = gz
		d.body = bufio.NewReaderSize(gz, 64<<10)
	}
	return d, nil
}

func (o Options) mysql(ctx context.Context, stdin io.Reader, stderr io.Writer, extra ...string) error {
	cmd := exec.CommandContext(ctx, o.Bin, append(slices.Clone(o.Args), extra...)...)
	cmd.Stdin = stdin
	cmd.Stdout = o.Stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	return cmd.Run()
}

// Run optionally (re)creates the database, then imports the dump into it.
func Run(ctx context.Context, o Options) (Result, error) {
	if _, err := exec.LookPath(o.Bin); err != nil {
		return Result{}, fmt.Errorf("%w: %q is not in PATH", ErrNoClient, o.Bin)
	}

	// open before touching the database so a bad file never costs a drop
	s, err := openStream(o.File, o.TimeZone)
	if err != nil {
		return Result{}, err
	}
	defer s.Close()

	// index before touching the database too: a corrupt dump must fail before a drop
	var p *plan
	if o.Parallel >= 2 {
		var w io.Writer
		if o.Progress {
			w = o.Stderr
		}
		if p, err = index(ctx, o.File, w); err != nil {
			return Result{}, fmt.Errorf("index dump: %w", err)
		}
		if p.serial != "" {
			fmt.Fprintf(o.Stderr, "Parallel import is not possible: %s. Importing serially.\n", p.serial)
			p = nil
		}
	}

	q := quoteIdent(o.DB)
	switch {
	case o.Fresh:
		err = o.mysql(ctx, nil, o.Stderr, "-e", "DROP DATABASE IF EXISTS "+q+"; CREATE DATABASE "+q)
	case o.Create:
		err = o.mysql(ctx, nil, o.Stderr, "-e", "CREATE DATABASE IF NOT EXISTS "+q)
	}
	if err != nil {
		return Result{}, fmt.Errorf("prepare database %s: %w", o.DB, err)
	}

	start := time.Now()
	stderr := o.Stderr
	n, size := &s.n.n, s.size
	var done atomic.Int64
	var cur atomic.Pointer[string]
	if p != nil {
		n, size = &done, p.total()
	}

	var stop func()
	if o.Progress {
		b := &bar{w: o.Stderr, n: n, size: size, start: start, cur: &cur}
		stderr = b
		stop = b.start200ms()
	}

	// --database= instead of a positional arg so names starting with "-" are safe
	ts := &tableScan{r: s, cur: &cur}
	if p != nil {
		err = runParallel(ctx, o, p, stderr, &done, &cur)
	} else {
		err = o.mysql(ctx, ts, stderr, "--database="+o.DB)
	}
	if stop != nil {
		stop()
	}

	// ts.n is safe to read: cmd.Run waits for the goroutine copying stdin
	r := Result{Bytes: s.n.n.Load(), Tables: ts.n, Elapsed: time.Since(start)}
	if p != nil {
		r.Bytes, r.Tables = s.size, p.tables
	}
	if err != nil {
		return r, fmt.Errorf("import into %s failed: %w", o.DB, err)
	}
	return r, nil
}

// Databases lists the databases visible to the configured mysql user.
func Databases(ctx context.Context, bin string, args []string) ([]string, error) {
	out, err := exec.CommandContext(ctx, bin, append(slices.Clone(args), "-N", "-B", "-e", "SHOW DATABASES")...).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// DBFromFile derives a database name from a dump path: /x/myapp.sql.gz -> myapp.
func DBFromFile(path string) string {
	b := filepath.Base(path)
	b = strings.TrimSuffix(b, ".gz")
	return strings.TrimSuffix(b, ".sql")
}
