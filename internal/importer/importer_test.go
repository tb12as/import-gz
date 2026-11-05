package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

const body = "CREATE TABLE t (id int);\nINSERT INTO t VALUES (1);"

func writeFile(t *testing.T, name string, gz bool) string {
	t.Helper()

	var b bytes.Buffer
	if gz {
		w := gzip.NewWriter(&b)
		w.Write([]byte(body))
		w.Close()
	} else {
		b.WriteString(body)
	}

	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenStream(t *testing.T) {
	for _, gz := range []bool{true, false} {
		p := writeFile(t, "dump.sql.gz", gz)

		s, err := openStream(p, "")
		if err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(s)
		s.Close()
		if err != nil {
			t.Fatal(err)
		}
		if want := prefix + body + suffix; string(got) != want {
			t.Errorf("gz=%v: got %q, want %q", gz, got, want)
		}
		if s.n.n.Load() != s.size {
			t.Errorf("gz=%v: counted %d bytes, file has %d", gz, s.n.n.Load(), s.size)
		}
	}
}

func TestOpenStreamDir(t *testing.T) {
	if _, err := openStream(t.TempDir(), ""); err == nil {
		t.Error("expected error for directory")
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("got %s", got)
	}
}

func TestDBFromFile(t *testing.T) {
	cases := map[string]string{
		"/x/myapp.sql.gz": "myapp",
		"myapp.sql":       "myapp",
		"myapp.gz":        "myapp",
		"a.b.sql.gz":      "a.b",
		".sql.gz":         "",
	}
	for in, want := range cases {
		if got := DBFromFile(in); got != want {
			t.Errorf("DBFromFile(%q) = %q, want %q", in, got, want)
		}
	}
}

// fake mysql client: records args and stdin
func fakeClient(t *testing.T, exit int) (bin, dir string) {
	t.Helper()

	dir = t.TempDir()
	bin = filepath.Join(dir, "mysql")
	script := "#!/bin/sh\necho \"$@\" >> \"" + dir + "/args\"\ncat >> \"" + dir + "/stdin\"\nexit " +
		string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestRun(t *testing.T) {
	bin, dir := fakeClient(t, 0)
	p := writeFile(t, "dump.sql.gz", true)

	_, err := Run(context.Background(), Options{
		File: p, DB: "my`db", Bin: bin, Args: []string{"-h", "x"}, Fresh: true,
		Stdout: io.Discard, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	want := "-h x -e DROP DATABASE IF EXISTS `my``db`; CREATE DATABASE `my``db`\n-h x --database=my`db\n"
	if string(args) != want {
		t.Errorf("args:\n%s\nwant:\n%s", args, want)
	}

	in, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.Contains(string(in), prefix+body+suffix) {
		t.Errorf("stdin = %q", in)
	}
}

func TestRunExitCode(t *testing.T) {
	bin, _ := fakeClient(t, 3)
	p := writeFile(t, "dump.sql", false)

	_, err := Run(context.Background(), Options{File: p, DB: "d", Bin: bin, Stdout: io.Discard, Stderr: io.Discard})
	if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 3 {
		t.Errorf("want exit code 3, got %v", err)
	}
}

func TestRunNoClient(t *testing.T) {
	_, err := Run(context.Background(), Options{File: "x", DB: "d", Bin: "no-such-mysql-bin"})
	if !errors.Is(err, ErrNoClient) {
		t.Errorf("got %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB"}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTableScan(t *testing.T) {
	f, err := os.Open("testdata/mysqldump.sql")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// one byte per read splits every line and name across reads
	var cur atomic.Pointer[string]
	ts := &tableScan{r: iotest.OneByteReader(f), cur: &cur}

	var seen []string
	buf := make([]byte, 1)
	for {
		_, err := ts.Read(buf)
		if s := cur.Load(); s != nil && (len(seen) == 0 || seen[len(seen)-1] != *s) {
			seen = append(seen, *s)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// the dump also holds 'CREATE TABLE fake;' inside a string, which must not count
	if got := strings.Join(seen, ","); got != "audit,child,parent" || ts.n != 3 {
		t.Errorf("seen %q, n %d", got, ts.n)
	}
}
