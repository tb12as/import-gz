package importer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const (
	adminerHead   = "-- Adminer 6.1.2-dev MySQL 8.0.41 dump\n\nSET NAMES utf8;\nSET time_zone = '+00:00';\n\n"
	mysqldumpHead = "-- MySQL dump 10.13\n/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;\n" +
		"/*!40103 SET TIME_ZONE='+00:00' */;\n\n"
	rows = "DROP TABLE IF EXISTS `s`;\nCREATE TABLE `s` (`id` int, `at` timestamp NULL, `note` text);\n" +
		"INSERT INTO `s` VALUES (1,'2024-03-31 14:57:02','SET time_zone = ''+00:00'''),\n" +
		"(2,'2026-11-04 11:11:46.123456',NULL);\n"
)

func gzFile(t *testing.T, s string, mod time.Time) string {
	t.Helper()

	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	io.WriteString(w, s)
	w.Close()

	p := filepath.Join(t.TempDir(), "d.sql.gz")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		os.Chtimes(p, mod, mod)
	}
	return p
}

func TestDetectLocalTZ(t *testing.T) {
	const off = 7 * time.Hour
	dumped := time.Date(2026, 11, 4, 4, 12, 1, 0, time.UTC)
	footer := "\n-- 2026-11-04 04:12:01 UTC\n"

	cases := []struct {
		name string
		dump string
		mod  time.Time
		want bool
	}{
		{"adminer, values newer than dump", adminerHead + rows + footer, time.Time{}, true},
		{"adminer, true utc values", adminerHead + strings.ReplaceAll(rows, "2026-11-04 11", "2026-11-04 03") +
			footer, time.Time{}, false},
		{"mysqldump uses mtime", mysqldumpHead + rows, dumped, true},
		{"mysqldump, downloaded much later", mysqldumpHead + rows, dumped.Add(30 * 24 * time.Hour), false},
		{"header not utc", "SET time_zone = '+07:00';\n" + rows + footer, time.Time{}, false},
		{"no header", rows + footer, time.Time{}, false},
	}

	for _, c := range cases {
		got, err := DetectLocalTZ(context.Background(), gzFile(t, c.dump, c.mod), off, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// negative and zero offsets are never detected
	if got, _ := DetectLocalTZ(context.Background(), gzFile(t, adminerHead+rows+footer, time.Time{}), 0, nil); got {
		t.Error("offset 0 detected")
	}
}

func TestScannerBoundaries(t *testing.T) {
	in := "x'2026-11-04 11:11:46' '2020-01-01 00:00:00.5' 'nope' '2021-13-0' '2022-02-02 02:02:02'\n-- last line"

	var s dtScanner
	if err := s.scan(context.Background(), iotest.OneByteReader(strings.NewReader(in))); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"2026-11-04 11", "2020-01-01 00", "2022-02-02 02"} {
		if s.hours[k] == nil {
			t.Errorf("missing %s, got %v", k, s.hours)
		}
	}
	if len(s.hours) != 3 {
		t.Errorf("got %d hours, want 3", len(s.hours))
	}
	if got := string(s.lastLine()); got != "-- last line" {
		t.Errorf("last line %q", got)
	}
}

func TestWithin(t *testing.T) {
	var s dtScanner
	s.record([]byte("2026-11-04 04:12:01"))
	s.record([]byte("2026-11-04 11:12:02"))

	lo := time.Date(2026, 11, 4, 4, 12, 1, 0, time.UTC)
	if s.within(lo, lo.Add(7*time.Hour)) {
		t.Error("values at lo and just past hi must not count")
	}

	s.record([]byte("2026-11-04 04:12:02"))
	if !s.within(lo, lo.Add(7*time.Hour)) {
		t.Error("value just after lo must count")
	}
}

func TestHeaderRewrite(t *testing.T) {
	for _, head := range []string{adminerHead, mysqldumpHead} {
		p := gzFile(t, head+rows, time.Time{})
		s, err := openStream(p, "+07:00")
		if err != nil {
			t.Fatal(err)
		}

		b, _ := io.ReadAll(s)
		s.Close()
		got := string(b)

		if strings.Contains(strings.ToLower(got[:strings.Index(got, "DROP")]), "'+00:00'") {
			t.Errorf("header still utc:\n%s", got)
		}
		if !strings.HasPrefix(got, prefix+"SET time_zone = '+07:00';\n") {
			t.Errorf("missing session tz:\n%s", got)
		}
		// data rows are never rewritten, and @@TIME_ZONE references stay intact
		if !strings.Contains(got, rows) {
			t.Errorf("rows changed:\n%s", got)
		}
		if head == mysqldumpHead && !strings.Contains(got, "@OLD_TIME_ZONE=@@TIME_ZONE") {
			t.Errorf("save line changed:\n%s", got)
		}
	}

	if _, err := openStream(gzFile(t, rows, time.Time{}), "+07:00'; DROP DATABASE x; --"); err == nil {
		t.Error("unsafe tz accepted")
	}
}

func TestHeaderStopsAtStatement(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(adminerHead + rows))
	if h := string(header(br)); h != adminerHead {
		t.Errorf("header %q", h)
	}
}

func TestValidTZ(t *testing.T) {
	for s, want := range map[string]bool{"+07:00": true, "-03:30": true, "SYSTEM": true, "+7:00": false,
		"+15:00": false, "UTC": false, "+07:00'": false} {
		if ValidTZ(s) != want {
			t.Errorf("ValidTZ(%q) != %v", s, want)
		}
	}
}
