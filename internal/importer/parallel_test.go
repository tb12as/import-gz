package importer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustIndex(t *testing.T, path string) (*plan, string) {
	t.Helper()

	p, err := index(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return p, string(raw)
}

// every byte of the dump lands in exactly one phase
func checkCoverage(t *testing.T, p *plan, raw string) {
	t.Helper()

	n := int64(len(p.header) + len(p.ddl))
	for _, s := range p.data {
		n += s.end - s.start
	}
	if p.tail >= 0 {
		n += p.size - p.tail
	}
	if n != int64(len(raw)) || p.size != int64(len(raw)) {
		t.Errorf("phases cover %d bytes, size %d, dump has %d", n, p.size, len(raw))
	}
}

func TestIndexMysqldump(t *testing.T) {
	for file, tailStart := range map[string]string{
		"testdata/mysqldump.sql":            "-- Dumping events",
		"testdata/mysqldump_nocomments.sql": "/*!50003 DROP PROCEDURE",
	} {
		p, raw := mustIndex(t, file)
		if p.serial != "" {
			t.Fatalf("%s: serial: %s", file, p.serial)
		}
		checkCoverage(t, p, raw)

		if !strings.Contains(string(p.header), "SET TIME_ZONE='+00:00'") || strings.Contains(string(p.header), "TABLE") {
			t.Errorf("%s: header:\n%s", file, p.header)
		}

		ddl := string(p.ddl)
		for _, want := range []string{"CREATE TABLE `audit`", "CREATE TABLE `child`", "CREATE TABLE `parent`",
			"/*!50001 CREATE VIEW `v_child`", "SET character_set_client = @saved_cs_client"} {
			if !strings.Contains(ddl, want) {
				t.Errorf("%s: ddl lacks %q", file, want)
			}
		}
		if strings.Contains(ddl, "INSERT") || strings.Contains(ddl, "TRIGGER") {
			t.Errorf("%s: ddl has data or triggers:\n%s", file, ddl)
		}

		// placeholder view has no data, so three tables carry data
		if len(p.data) != 3 {
			t.Fatalf("%s: %d data ranges, want 3", file, len(p.data))
		}
		if p.tables != 3 {
			t.Errorf("%s: %d tables, want 3", file, p.tables)
		}
		for i, tbl := range []string{"audit", "child", "parent"} {
			if p.data[i].name != tbl {
				t.Errorf("%s: range %d name %q, want %q", file, i, p.data[i].name, tbl)
			}
			d := raw[p.data[i].start:p.data[i].end]
			if strings.Count(d, "INSERT INTO `") != 1 || !strings.Contains(d, "INSERT INTO `"+tbl+"`") {
				t.Errorf("%s: range %d is not %s's data:\n%s", file, i, tbl, d)
			}
		}
		if !strings.Contains(raw[p.data[1].start:p.data[1].end], "TRIGGER `child_ai`") {
			t.Errorf("%s: trigger is not with child's data", file)
		}

		tail := raw[p.tail:]
		if !strings.HasPrefix(tail, tailStart) || !strings.Contains(tail, "CREATE ALGORITHM") ||
			!strings.Contains(tail, "PROCEDURE `p_count`") {
			t.Errorf("%s: tail:\n%s", file, tail)
		}
	}
}

const adminerDump = "-- Adminer 5.0.0 MySQL 8.0.41 dump\n\nSET NAMES utf8;\nSET time_zone = '+00:00';\n" +
	"SET foreign_key_checks = 0;\n\nDROP TABLE IF EXISTS `a`;\nCREATE TABLE `a` (\n  `id` int NOT NULL\n) " +
	"ENGINE=InnoDB;\n\nTRUNCATE `a`;\nINSERT INTO `a` (`id`) VALUES\n(1),\n(2);\n\nDELIMITER ;;\n" +
	"CREATE TRIGGER `a_bi` BEFORE INSERT ON `a` FOR EACH ROW SET NEW.id = NEW.id;;\nDELIMITER ;\n\n" +
	"DROP TABLE IF EXISTS `empty`;\nCREATE TABLE `empty` (`id` int) ENGINE=InnoDB;\n\n" +
	"DROP TABLE IF EXISTS `b`;\nCREATE TABLE `b` (`id` int) ENGINE=InnoDB;\n\nINSERT INTO `b` VALUES (3);\n\n" +
	"DROP VIEW IF EXISTS `v`;\nCREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW `v` AS select 1;\n\n" +
	"-- 2026-11-04 04:12:01 UTC\n"

func TestIndexAdminer(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.sql")
	os.WriteFile(p, []byte(adminerDump), 0o644)

	pl, raw := mustIndex(t, p)
	if pl.serial != "" {
		t.Fatal(pl.serial)
	}
	checkCoverage(t, pl, raw)

	if len(pl.data) != 2 {
		t.Fatalf("%d data ranges, want 2", len(pl.data))
	}
	if pl.tables != 3 || pl.data[0].name != "a" || pl.data[1].name != "b" {
		t.Errorf("tables %d, names %q %q", pl.tables, pl.data[0].name, pl.data[1].name)
	}
	a, b := raw[pl.data[0].start:pl.data[0].end], raw[pl.data[1].start:pl.data[1].end]
	if !strings.HasPrefix(a, "TRUNCATE `a`") || !strings.Contains(a, "(2);") || !strings.Contains(a, "a_bi") {
		t.Errorf("a's data:\n%s", a)
	}
	if !strings.HasPrefix(b, "INSERT INTO `b`") {
		t.Errorf("b's data:\n%s", b)
	}
	if !strings.Contains(string(pl.ddl), "CREATE TABLE `empty`") {
		t.Errorf("ddl:\n%s", pl.ddl)
	}
	if !strings.HasPrefix(raw[pl.tail:], "DROP VIEW") {
		t.Errorf("tail:\n%s", raw[pl.tail:])
	}
}

func TestIndexSerial(t *testing.T) {
	cases := map[string]string{
		"drops db in header": "/*!40000 DROP DATABASE IF EXISTS `x`*/;\nCREATE TABLE `t` (`id` int);\n",
		"no tables":          "SET NAMES utf8;\nINSERT INTO `t` VALUES (1);\n",
	}
	for name, dump := range cases {
		p := filepath.Join(t.TempDir(), "d.sql")
		os.WriteFile(p, []byte(dump), 0o644)

		pl, _ := mustIndex(t, p)
		if pl.serial == "" {
			t.Errorf("%s: expected serial fallback", name)
		}
	}
}

// long lines larger than the read buffer are copied whole into the right phase
func TestIndexLongLines(t *testing.T) {
	long := "INSERT INTO `t` VALUES " + strings.Repeat("(1,'"+strings.Repeat("x", 1000)+"'),", 300) + "(2,'y');\n"
	comment := "-- " + strings.Repeat("c", 100<<10) + "\n"
	dump := "SET NAMES utf8;\n" + comment + "CREATE TABLE `t` (`id` int);\n" + long + "CREATE TABLE `u` (`id` int);\n"

	p := filepath.Join(t.TempDir(), "d.sql.gz")
	os.WriteFile(p, gzBytes(t, dump), 0o644)

	pl, err := index(context.Background(), p, nil)
	if err != nil || pl.serial != "" {
		t.Fatal(err, pl.serial)
	}
	if string(pl.header) != "SET NAMES utf8;\n"+comment {
		t.Errorf("header has %d bytes", len(pl.header))
	}
	if len(pl.data) != 1 || pl.data[0].end-pl.data[0].start != int64(len(long)) {
		t.Errorf("data %v, want one range of %d bytes", pl.data, len(long))
	}
	if string(pl.ddl) != "CREATE TABLE `t` (`id` int);\nCREATE TABLE `u` (`id` int);\n" {
		t.Errorf("ddl %q", pl.ddl)
	}
}

func TestRunParallel(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "mysql")
	script := "#!/bin/sh\ncat > \"" + dir + "/stdin.$$\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile("testdata/mysqldump.sql")
	p := filepath.Join(t.TempDir(), "d.sql.gz")
	os.WriteFile(p, gzBytes(t, string(raw)), 0o644)

	_, err := Run(context.Background(), Options{File: p, DB: "d", Bin: bin, Parallel: 2, TimeZone: "+07:00",
		Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "stdin.*"))
	if len(files) != 4 { // tables, 2 workers, tail
		t.Fatalf("%d sessions, want 4", len(files))
	}

	var all strings.Builder
	for _, f := range files {
		b, _ := os.ReadFile(f)
		s := string(b)
		if !strings.HasPrefix(s, sessionPrefix("+07:00")) || !strings.Contains(s, "SET TIME_ZONE='+07:00'") ||
			!strings.HasSuffix(s, suffix) {
			t.Errorf("session %s lacks prefix, rewritten header or suffix:\n%s", f, s)
		}
		all.WriteString(s)
	}

	for _, tbl := range []string{"audit", "child", "parent"} {
		if n := strings.Count(all.String(), "INSERT INTO `"+tbl+"`"); n != 1 {
			t.Errorf("%s inserted %d times", tbl, n)
		}
	}
}

func gzBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := os.ReadFile(gzFile(t, s, time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTableName(t *testing.T) {
	for line, want := range map[string]string{
		"CREATE TABLE `users` (\n":              "users",
		"CREATE TABLE IF NOT EXISTS `a``b` (":   "a`b",
		"create table if not exists orders(id)": "orders",
		"CREATE TABLE plain (\n":                "plain",
	} {
		if got := tableName([]byte(line)); got != want {
			t.Errorf("tableName(%q) = %q, want %q", line, got, want)
		}
	}
}
