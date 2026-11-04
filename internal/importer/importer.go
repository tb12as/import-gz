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
	Stdout   io.Writer
	Stderr   io.Writer
}

type Result struct {
	Bytes   int64 // bytes read from the dump file
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

	pre := prefix
	var body io.Reader = d.body
	if tz != "" {
		pre += "SET time_zone = '" + tz + "';\n"
		h := tzRe.ReplaceAll(header(d.body), []byte("${1}'"+tz+"'"))
		body = io.MultiReader(bytes.NewReader(h), d.body)
	}

	return &stream{io.MultiReader(strings.NewReader(pre), body, strings.NewReader(suffix)), d}, nil
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
	var stop func()
	if o.Progress {
		b := &bar{w: o.Stderr, n: s.n, size: s.size, start: start}
		stderr = b
		stop = b.start200ms()
	}

	// --database= instead of a positional arg so names starting with "-" are safe
	err = o.mysql(ctx, s, stderr, "--database="+o.DB)
	if stop != nil {
		stop()
	}

	r := Result{Bytes: s.n.n.Load(), Elapsed: time.Since(start)}
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
