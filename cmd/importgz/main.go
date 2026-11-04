// Command importgz imports a .sql.gz (or .sql) dump into a local MySQL database.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/tb12as/import-gz/internal/config"
	"github.com/tb12as/import-gz/internal/importer"
	"github.com/tb12as/import-gz/internal/prompt"
)

const usage = `Usage: importgz [flags] [dump.sql.gz|dump.sql] [database] [-- mysql args]
       importgz config [-show | -clear]

Imports a dump into a MySQL database with foreign key checks disabled.
In a terminal, a missing file or database is asked for interactively, with suggestions.
Otherwise a missing database is derived from the file name (myapp.sql.gz -> myapp).
Credentials: -u/-p/-host/-port flags, then settings saved with "importgz config", then ~/.my.cnf.
Arguments after -- are passed to every mysql call (e.g. -- --socket=/tmp/mysql.sock).

Flags:
`

type painter bool

func (p painter) paint(code, s string) string {
	if !p {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// isTTY uses a terminal ioctl; a mode check would also accept /dev/null
func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// offset formats seconds east of UTC as +07:00
func offset(secs int) string {
	sign := '+'
	if secs < 0 {
		sign, secs = '-', -secs
	}
	return fmt.Sprintf("%c%02d:%02d", sign, secs/3600, secs%3600/60)
}

func promptErr(err error, fail func(string, ...any)) int {
	if errors.Is(err, prompt.ErrCancelled) {
		fmt.Fprintln(os.Stderr, "Aborted.")
		return 130
	}
	fail("%v", err)
	return 1
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "config" {
		return runConfig(args[1:])
	}

	fs := flag.NewFlagSet("importgz", flag.ContinueOnError)
	user := fs.String("u", "", "MySQL user")
	askPw := fs.Bool("p", false, "ask for the MySQL password (hidden input)")
	host := fs.String("host", "", "MySQL host")
	port := fs.Int("port", 0, "MySQL port")
	create := fs.Bool("create", false, "create the database if it does not exist")
	fresh := fs.Bool("fresh", false, "drop and recreate the database before importing")
	yes := fs.Bool("y", false, "do not ask for confirmation with -fresh")
	quiet := fs.Bool("q", false, "do not show the progress bar")
	bin := fs.String("mysql", "mysql", "mysql client binary")
	tzMode := fs.String("tz", "auto", "time_zone for TIMESTAMP values: auto (fix local-time values in a dump\n"+
		"labelled UTC), keep (use the dump header), or an offset such as +07:00")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
	}

	var extra []string
	if i := slices.Index(args, "--"); i >= 0 {
		extra, args = args[i+1:], args[:i]
	}

	// allow flags before, between and after positional args
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}

	p := painter(isTTY(os.Stderr) && os.Getenv("NO_COLOR") == "")
	fail := func(format string, a ...any) {
		fmt.Fprintln(os.Stderr, p.paint("41", " Error ")+" "+fmt.Sprintf(format, a...))
	}

	if *tzMode != "auto" && *tzMode != "keep" && !importer.ValidTZ(*tzMode) {
		fail("invalid -tz %q; use auto, keep, SYSTEM or an offset such as +07:00", *tzMode)
		return 2
	}

	if *port < 0 || *port > 65535 {
		fail("invalid -port %d", *port)
		return 2
	}

	interactive := isTTY(os.Stdin) && isTTY(os.Stderr)

	saved, err := config.Load()
	if err != nil {
		fail("load config: %v", err)
		return 1
	}

	flags := config.Config{User: *user, Host: *host, Port: *port}
	if *askPw {
		if !isTTY(os.Stdin) {
			fail("-p needs a terminal to ask for the password; save it with \"importgz config\" instead")
			return 1
		}
		if flags.Password, err = readPassword("Password: "); err != nil {
			fail("%v", err)
			return 1
		}
	}

	opt, cleanup, err := config.OptionFile(config.Merge(saved, flags))
	if err != nil {
		fail("%v", err)
		return 1
	}
	defer cleanup()

	// --defaults-file must be mysql's first argument
	if opt != "" {
		extra = append([]string{"--defaults-file=" + opt}, extra...)
	}
	if len(pos) > 2 || len(pos) == 0 && !interactive {
		fs.Usage()
		return 2
	}

	var file, db string
	if len(pos) > 0 {
		file = pos[0]
	}
	if len(pos) > 1 {
		db = pos[1]
	}

	if file == "" {
		f, err := prompt.File()
		if err != nil {
			return promptErr(err, fail)
		}
		file = f
	}

	st, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		fail("file %q does not exist", file)
		return 1
	}
	if err != nil {
		fail("%v", err)
		return 1
	}
	if !st.Mode().IsRegular() {
		fail("%q is not a regular file", file)
		return 1
	}

	if db == "" && interactive {
		lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
		known, _ := importer.Databases(lctx, *bin, extra) // no suggestions if the server is unreachable
		lcancel()

		d, err := prompt.Database(importer.DBFromFile(file), known)
		if err != nil {
			return promptErr(err, fail)
		}
		db = d

		if known != nil && !slices.Contains(known, db) && !*fresh {
			*create = true
		}
	}

	if db == "" {
		db = importer.DBFromFile(file)
	}
	if db == "" {
		fail("cannot derive a database name from %q; pass it as the second argument", file)
		return 2
	}

	if *fresh && !*yes {
		if !isTTY(os.Stdin) {
			fail("-fresh drops database %q; pass -y to confirm when not running interactively", db)
			return 1
		}

		fmt.Fprintf(os.Stderr, "This drops database %s and all of its data. Continue? [y/N] ", p.paint("1", db))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return 1
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	progress := !*quiet && isTTY(os.Stderr)

	tz := *tzMode
	switch tz {
	case "keep":
		tz = ""
	case "auto":
		tz = ""
		_, secs := time.Now().Zone()
		var w io.Writer
		if progress {
			w = os.Stderr
		}

		local, err := importer.DetectLocalTZ(ctx, file, time.Duration(secs)*time.Second, w)
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "Aborted.")
			return 130
		}
		if err != nil {
			fail("%v", err)
			return 1
		}

		if local {
			tz = offset(secs)
			fmt.Fprintf(os.Stderr, "%s local times in UTC dump, using %s (-tz keep to skip)\n",
				p.paint("43;30", " Timezone "), tz)
		}
	}

	fmt.Fprintf(os.Stderr, "Importing %s (%s) into %s\n", file, importer.HumanBytes(st.Size()), p.paint("1", db))

	res, err := importer.Run(ctx, importer.Options{
		File:     file,
		DB:       db,
		Bin:      *bin,
		Args:     extra,
		Create:   *create,
		Fresh:    *fresh,
		TimeZone: tz,
		Progress: progress,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	})

	if ctx.Err() != nil {
		fail("import cancelled after %s; database %q may be partially imported", fmtDur(res.Elapsed), db)
		return 130
	}

	if errors.Is(err, importer.ErrNoClient) {
		fail("%v (install the mysql client or pass -mysql <path>)", err)
		return 1
	}

	if err != nil {
		fail("%v (after %s)", err, fmtDur(res.Elapsed))
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() > 0 {
			return ee.ExitCode()
		}
		return 1
	}

	rate := float64(res.Bytes) / max(res.Elapsed.Seconds(), 1e-9)
	fmt.Fprintf(os.Stderr, "%s %s into %s in %s (%s, %s/s)\n",
		p.paint("44", " Imported "), file, p.paint("42", " "+db+" "), fmtDur(res.Elapsed),
		importer.HumanBytes(res.Bytes), importer.HumanBytes(int64(rate)))
	return 0
}
