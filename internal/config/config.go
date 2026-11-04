// Package config stores connection settings and hands them to the mysql client.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	User     string
	Password string
	Host     string
	Port     int
}

func (c Config) empty() bool { return c == Config{} }

// Path is the saved config location, e.g. ~/.config/importgz/my.cnf.
func Path() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "importgz", "my.cnf"), nil
}

// Merge returns saved with every field set in flags overriding it.
func Merge(saved, flags Config) Config {
	if flags.User != "" {
		saved.User = flags.User
	}
	if flags.Password != "" {
		saved.Password = flags.Password
	}
	if flags.Host != "" {
		saved.Host = flags.Host
	}
	if flags.Port != 0 {
		saved.Port = flags.Port
	}
	return saved
}

func quote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(v[1 : len(v)-1])
}

// section renders c as a [client] option group; values are quoted the way mysql parses them.
func (c Config) section() (string, error) {
	var b strings.Builder
	b.WriteString("[client]\n")

	for _, kv := range [][2]string{{"user", c.User}, {"password", c.Password}, {"host", c.Host}} {
		if kv[1] == "" {
			continue
		}
		if strings.ContainsAny(kv[1], "\r\n") {
			return "", fmt.Errorf("%s must not contain a line break", kv[0])
		}
		b.WriteString(kv[0] + "=" + quote(kv[1]) + "\n")
	}

	if c.Port != 0 {
		b.WriteString("port=" + strconv.Itoa(c.Port) + "\n")
	}
	return b.String(), nil
}

// Load reads the saved config; a missing file gives an empty Config.
func Load() (Config, error) {
	var c Config
	p, err := Path()
	if err != nil {
		return c, err
	}

	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}

		v = unquote(strings.TrimSpace(v))
		switch strings.TrimSpace(k) {
		case "user":
			c.User = v
		case "password":
			c.Password = v
		case "host":
			c.Host = v
		case "port":
			if c.Port, err = strconv.Atoi(v); err != nil {
				return c, fmt.Errorf("%s: invalid port %q", p, v)
			}
		}
	}
	return c, sc.Err()
}

// Save writes c to Path with owner-only permissions.
func Save(c Config) error {
	s, err := c.section()
	if err != nil {
		return err
	}

	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}

	// write to a temp file and rename so a failed write never leaves a half config
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Clear deletes the saved config.
func Clear() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// defaultFiles are mysql's standard option files; --defaults-file skips them, so they are included.
var defaultFiles = func() []string {
	fs := []string{"/etc/my.cnf", "/etc/mysql/my.cnf"}
	if h, err := os.UserHomeDir(); err == nil {
		fs = append(fs, filepath.Join(h, ".my.cnf"))
	}
	return fs
}

// OptionFile writes a temp option file for --defaults-file: the standard files first, then c, so c
// overrides ~/.my.cnf while other settings (socket, charset) still apply. It returns "" when c is empty.
// The password goes through this 0600 file instead of argv, so it never shows up in ps.
func OptionFile(c Config) (path string, cleanup func(), err error) {
	if c.empty() {
		return "", func() {}, nil
	}

	s, err := c.section()
	if err != nil {
		return "", nil, err
	}

	var b strings.Builder
	for _, p := range defaultFiles() {
		if _, err := os.Stat(p); err == nil {
			b.WriteString("!include " + p + "\n")
		}
	}
	b.WriteString(s)

	f, err := os.CreateTemp("", "importgz-*.cnf") // CreateTemp uses mode 0600
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
