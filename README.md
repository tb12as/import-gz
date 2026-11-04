# importgz

`importgz` imports a MySQL dump (`.sql.gz` or `.sql`) into a local database. It streams the dump into the
`mysql` client, so credentials and connection settings are read from `~/.my.cnf` exactly as the `mysql`
command reads them.

## Installation

```sh
go install github.com/tb12as/import-gz/cmd/importgz@latest
# or, from a checkout
go build -o bin/importgz ./cmd/importgz
```

The `mysql` client (or a compatible client such as `mariadb`) must be installed.

## Usage

```
importgz [flags] [dump.sql.gz|dump.sql] [database] [-- mysql args]
importgz config [-show | -clear]
```

| Flag            | Description                                                         |
|-----------------|---------------------------------------------------------------------|
| `-u <user>`     | Sets the MySQL user.                                                |
| `-p`            | Asks for the MySQL password with hidden input.                      |
| `-host <host>`  | Sets the MySQL host.                                                |
| `-port <port>`  | Sets the MySQL port.                                                |
| `-create`       | Creates the database if it does not exist.                          |
| `-fresh`        | Drops and recreates the database before importing. Asks to confirm. |
| `-y`            | Skips the confirmation prompt of `-fresh`.                          |
| `-q`            | Hides the progress bar.                                             |
| `-mysql <bin>`  | Sets the client binary. The default is `mysql`.                     |
| `-version`      | Prints the version and exits.                                       |
| `-tz <mode>`    | Sets the time zone handling: `auto` (default), `keep`, or an offset. |

Flags may appear before, between, or after the positional arguments. All arguments after `--` are passed to
every `mysql` invocation.

### Examples

```sh
importgz                                     # interactive: pick a file, then a database
importgz ~/Downloads/shop.sql.gz             # interactive database prompt, default "shop"
importgz ~/Downloads/shop.sql.gz shop_dev    # no prompts
importgz -fresh -y dump.sql.gz shop_dev      # drop, recreate, import
importgz -u admin -p -host 127.0.0.1 -port 3307 dump.sql.gz shop
```

## Credentials

Connection settings are resolved in the following order, from highest to lowest priority:

1. The `-u`, `-p`, `-host`, and `-port` flags.
2. The settings saved with `importgz config`.
3. The standard MySQL option files (`/etc/my.cnf`, `/etc/mysql/my.cnf`, `~/.my.cnf`).

Arguments after `--` are passed to `mysql` on its command line and therefore override all of the above.

`importgz config` asks for the user, password (hidden input), host, and port, and stores them in
`~/.config/importgz/my.cnf` with mode `0600`. At each prompt, Enter keeps the current value and `-` clears it.
`importgz config -show` prints the saved settings with the password masked, and `importgz config -clear` deletes
the file.

```sh
importgz config                       # save credentials once
importgz dump.sql.gz shop             # uses the saved credentials
importgz -u root -p dump.sql.gz shop  # overrides them for one run
```

No flag accepts the password as a value, so it does not appear in the process list or the shell history. When
any setting is configured, `importgz` writes a temporary option file (mode `0600`) that includes the standard
option files followed by the configured values. It passes the file to `mysql` with `--defaults-file`, and
deletes it when the command exits. Other settings in `~/.my.cnf`, such as `socket`, remain in effect.

## Interactive mode

When standard input and standard error are both terminals, a missing argument is requested interactively.

- **Dump file.** Type a path. The list below the input shows matching `.sql.gz` and `.sql` files, newest
  first, followed by subdirectories. Typing an existing directory (for example `~/Downloads`) lists its
  contents. Use the arrow keys to move, Tab to complete the highlighted entry, Enter to confirm or to enter a
  directory, and Esc to cancel. Hidden entries are listed only when the typed name starts with a dot.
- **Database.** Existing databases are suggested and filtered as you type. Pressing Enter without a
  selection accepts the typed name, or the name derived from the file when the input is empty. A database that
  does not exist is created automatically.

When the session is not interactive, a missing database name is derived from the file name
(`myapp.sql.gz` becomes `myapp`), and a missing file argument is an error.

## Behavior

- The dump is wrapped in `SET FOREIGN_KEY_CHECKS=0; SET AUTOCOMMIT=0;` and `COMMIT; SET FOREIGN_KEY_CHECKS=1;`.
  Disabling autocommit avoids one commit per statement, which MySQL recommends for bulk loading. If an import
  fails or is cancelled, the uncommitted rows of the current table are rolled back.
- Gzip input is detected by its magic bytes, not by the file extension. Concatenated gzip members are
  supported.
- A progress bar on standard error shows the percentage, bytes read, throughput, and estimated time remaining.
  It is hidden when standard error is not a terminal.
- Colors are disabled when standard error is not a terminal or when `NO_COLOR` is set.
- The file and the gzip header are validated before any database is dropped or created.
- Ctrl+C stops the import and exits with status 130.

## Time zone correction

Some servers run MySQL in UTC while the application writes local wall-clock values (for example WIB, UTC+7)
into `TIMESTAMP` columns. The dump header then declares the values as UTC (`SET time_zone = '+00:00';` in
Adminer, `/*!40103 SET TIME_ZONE='+00:00' */;` in mysqldump), and an import shifts every `TIMESTAMP` value
by the local offset. `DATETIME` columns are not affected.

With `-tz auto`, the default, `importgz` detects this case before importing:

1. The header must declare `time_zone '+00:00'`, and the local offset must be positive.
2. The dump time is read from the Adminer footer (`-- 2026-11-04 04:12:01 UTC`). For other dumps, the file
   modification time is used.
3. Real UTC data is never newer than the dump. If any date-time literal falls between the dump time and the
   dump time plus the local offset, the values are local time.

When the case is detected, a notice is printed, the header is rewritten, and the session uses the local offset
(for example `time_zone = '+07:00'`). Only the header lines before the first statement are rewritten; data rows
are never modified. The check requires one additional read of the dump.

- `-tz keep` imports with the time zone declared in the dump.
- `-tz +07:00` (or `SYSTEM`) forces the given session time zone, regardless of the header.

Limitations: negative offsets cannot be detected. `DATETIME` literals are also inspected, so a dump with
local-time `DATETIME` columns but correct UTC `TIMESTAMP` columns is a false positive; use `-tz keep` for such
dumps. A fixed offset is applied, so zones with daylight saving time are corrected with the current offset.

## Exit status

| Status | Meaning                                                  |
|--------|----------------------------------------------------------|
| 0      | The import succeeded.                                    |
| 1      | A validation error occurred, or the user aborted `-fresh`. |
| 2      | The command line is invalid.                             |
| 130    | The import or a prompt was cancelled.                    |
| other  | The exit status of the `mysql` client.                   |

## Project layout

```
cmd/importgz/        command-line entry point: flags, prompts, output
internal/config/     saved credentials and the mysql option file
internal/importer/   dump streaming, mysql execution, progress bar, time zone detection
internal/prompt/     interactive file and database pickers
```

## Development

```sh
go vet ./...
go test ./...
```
