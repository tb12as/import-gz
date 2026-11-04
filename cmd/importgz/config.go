package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/tb12as/import-gz/internal/config"
)

const configUsage = `Usage: importgz config [-show | -clear]

Saves the MySQL user, password, host and port used by importgz. Saved values override ~/.my.cnf,
and the -u, -p, -host and -port flags override saved values. Without flags, asks for each value:
Enter keeps the current value, "-" clears it.

Flags:
`

func readPassword(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func show(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}

func runConfig(args []string) int {
	fs := flag.NewFlagSet("importgz config", flag.ContinueOnError)
	showOnly := fs.Bool("show", false, "print the saved settings")
	clear := fs.Bool("clear", false, "delete the saved settings")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, configUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	p, err := config.Path()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}

	if *clear {
		if err := config.Clear(); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "Removed", p)
		return 0
	}

	c, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}

	if *showOnly {
		pw, port := "", ""
		if c.Password != "" {
			pw = "********"
		}
		if c.Port != 0 {
			port = strconv.Itoa(c.Port)
		}
		fmt.Printf("file:     %s\nuser:     %s\npassword: %s\nhost:     %s\nport:     %s\n",
			p, show(c.User), show(pw), show(c.Host), show(port))
		return 0
	}

	if !isTTY(os.Stdin) {
		fmt.Fprintln(os.Stderr, "Error: importgz config needs a terminal; use -show or -clear otherwise")
		return 1
	}

	fmt.Fprintf(os.Stderr, "Settings are saved to %s. Enter keeps the current value, \"-\" clears it.\n", p)
	in := bufio.NewReader(os.Stdin)
	ask := func(label, cur string) string {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, cur)
		line, _ := in.ReadString('\n')
		switch v := strings.TrimSpace(line); v {
		case "":
			return cur
		case "-":
			return ""
		default:
			return v
		}
	}

	c.User = ask("User", c.User)

	cur := "not set"
	if c.Password != "" {
		cur = "set"
	}
	pw, err := readPassword("Password [" + cur + "]: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	switch pw {
	case "":
	case "-":
		c.Password = ""
	default:
		c.Password = pw
	}

	c.Host = ask("Host", c.Host)

	port := ""
	if c.Port != 0 {
		port = strconv.Itoa(c.Port)
	}
	for {
		v := ask("Port", port)
		if v == "" {
			c.Port = 0
			break
		}
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 && n < 65536 {
			c.Port = n
			break
		}
		fmt.Fprintln(os.Stderr, "Port must be a number from 1 to 65535.")
	}

	if err := config.Save(c); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Saved", p)
	return 0
}
