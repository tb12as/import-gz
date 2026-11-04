package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const nasty = `p a"s\s#w'd\`

func TestSaveLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if c, err := Load(); err != nil || !c.empty() {
		t.Fatalf("missing file: %+v %v", c, err)
	}

	want := Config{User: "admin", Password: nasty, Host: "127.0.0.1", Port: 3307}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}

	p, _ := Path()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}

	got, err := Load()
	if err != nil || got != want {
		t.Errorf("got %+v %v, want %+v", got, err, want)
	}

	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load(); !c.empty() {
		t.Error("clear left settings")
	}
	if err := Clear(); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
}

func TestSaveRejectsNewline(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := Save(Config{Password: "a\nhost=evil"}); err == nil {
		t.Error("newline accepted")
	}
}

func TestMerge(t *testing.T) {
	saved := Config{User: "a", Password: "pa", Host: "h", Port: 1}
	got := Merge(saved, Config{User: "b", Port: 2})
	if want := (Config{User: "b", Password: "pa", Host: "h", Port: 2}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestOptionFile(t *testing.T) {
	d := t.TempDir()
	mycnf := filepath.Join(d, ".my.cnf")
	os.WriteFile(mycnf, []byte("[client]\nuser=root\n"), 0o600)

	old := defaultFiles
	defaultFiles = func() []string { return []string{filepath.Join(d, "missing.cnf"), mycnf} }
	defer func() { defaultFiles = old }()

	p, cleanup, err := OptionFile(Config{})
	if err != nil || p != "" {
		t.Fatalf("empty config: %q %v", p, err)
	}
	cleanup()

	p, cleanup, err = OptionFile(Config{User: "admin", Password: nasty})
	if err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(p)
	want := "!include " + mycnf + "\n[client]\nuser=\"admin\"\npassword=" + quote(nasty) + "\n"
	if string(b) != want {
		t.Errorf("got:\n%s\nwant:\n%s", b, want)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}

	// mysql's own parser must read back the same values, with ours overriding ~/.my.cnf
	if out, err := exec.Command("my_print_defaults", "--defaults-file="+p, "-s", "client").Output(); err == nil {
		got := string(out)
		if !strings.HasSuffix(got, "--user=admin\n--password="+nasty+"\n") {
			t.Errorf("my_print_defaults:\n%s", got)
		}
	}

	cleanup()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("cleanup left the file")
	}
}
