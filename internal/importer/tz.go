package importer

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"regexp"
	"time"
)

var (
	tzRe     = regexp.MustCompile(`(?i)(time_zone\s*=\s*)'[^']*'`)
	utcRe    = regexp.MustCompile(`(?i)time_zone\s*=\s*'\+00:00'`)
	validRe  = regexp.MustCompile(`^(SYSTEM|[+-](0\d|1[0-4]):[0-5]\d)$`)
	footerRe = regexp.MustCompile(`^-- (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) UTC$`)
)

const dtLayout = "2006-01-02 15:04:05"

// ValidTZ reports whether s is a time_zone value safe to put in sql: SYSTEM or an offset like +07:00.
func ValidTZ(s string) bool {
	return validRe.MatchString(s)
}

// header consumes and returns the leading comment/SET lines of a dump; it stops at the first
// statement (DROP, CREATE, INSERT, ...) so data rows are never rewritten.
func header(br *bufio.Reader) []byte {
	p, _ := br.Peek(br.Size())
	n := 0
	for n < len(p) {
		j := bytes.IndexByte(p[n:], '\n')
		if j < 0 {
			break
		}

		l := bytes.TrimSpace(p[n : n+j])
		if len(l) > 0 && !bytes.HasPrefix(l, []byte("--")) && !bytes.HasPrefix(l, []byte("/*")) &&
			!bytes.HasPrefix(bytes.ToUpper(l), []byte("SET ")) {
			break
		}
		n += j + 1
	}

	h := bytes.Clone(p[:n])
	br.Discard(n)
	return h
}

// DetectLocalTZ reports whether a dump whose header says time_zone '+00:00' really holds wall-clock
// values of a zone off ahead of UTC. Real UTC data is never newer than the dump, while shifted values
// land in (dump time, dump time + off]. Dump time is the Adminer footer, else the file mtime.
// shortcut: DATETIME literals are scanned too (time_zone does not touch them); a dump with local
// DATETIME but true UTC TIMESTAMP columns gives a false positive, use -tz keep for those.
func DetectLocalTZ(ctx context.Context, path string, off time.Duration, progress io.Writer) (bool, error) {
	// only positive offsets are detectable: shifted values look "in the future"
	if off < time.Hour {
		return false, nil
	}

	d, err := openDump(path)
	if err != nil {
		return false, err
	}
	defer d.Close()

	if !utcRe.Match(header(d.body)) {
		return false, nil
	}

	if progress != nil {
		b := &bar{w: progress, n: d.n, size: d.size, start: time.Now(), label: "checking timestamps "}
		defer b.start200ms()()
	}

	var sc dtScanner
	if err := sc.scan(ctx, d.body); err != nil {
		return false, err
	}

	ref := d.mod
	if m := footerRe.FindSubmatch(sc.lastLine()); m != nil {
		if t, err := time.ParseInLocation(dtLayout, string(m[1]), time.UTC); err == nil {
			ref = t
		}
	}
	return sc.within(ref, ref.Add(off)), nil
}

// span holds the min and max literal seen within one hour
type span struct{ min, max string }

// dtScanner records 'YYYY-MM-DD HH:MM:SS' literals bucketed by hour, so one pass answers
// "is any literal in (lo, hi]" once the dump time (footer) is known at the end.
type dtScanner struct {
	hours map[string]*span
	tail  []byte
}

func isDT(b []byte) bool {
	for i, c := range b {
		switch i {
		case 4, 7:
			if c != '-' {
				return false
			}
		case 10:
			if c != ' ' {
				return false
			}
		case 13, 16:
			if c != ':' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func (s *dtScanner) record(b []byte) {
	if s.hours == nil {
		s.hours = map[string]*span{}
	}

	h := s.hours[string(b[:13])]
	if h == nil {
		v := string(b)
		s.hours[v[:13]] = &span{v, v}
		return
	}
	if string(b) < h.min {
		h.min = string(b)
	}
	if string(b) > h.max {
		h.max = string(b)
	}
}

func (s *dtScanner) scan(ctx context.Context, r io.Reader) error {
	const lit = 21 // quote + 19 chars + closing quote or fraction dot
	buf := make([]byte, 256<<10)
	carry := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, err := r.Read(buf[carry:])
		data := buf[:carry+n]

		cut := len(data)
		for i := 0; i < len(data); {
			j := bytes.IndexByte(data[i:], '\'')
			if j < 0 {
				break
			}
			i += j

			// literal may continue in the next read
			if len(data)-i < lit && err == nil {
				cut = i
				break
			}
			if len(data)-i >= lit && isDT(data[i+1:i+20]) && (data[i+20] == '\'' || data[i+20] == '.') {
				s.record(data[i+1 : i+20])
				i += lit
				continue
			}
			i++
		}

		s.keepTail(data[:cut])
		carry = copy(buf, data[cut:])

		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// keepTail keeps the last bytes of the stream for footer parsing.
func (s *dtScanner) keepTail(p []byte) {
	const max = 256
	if len(p) >= max {
		s.tail = append(s.tail[:0], p[len(p)-max:]...)
		return
	}
	s.tail = append(s.tail, p...)
	if len(s.tail) > max {
		s.tail = s.tail[len(s.tail)-max:]
	}
}

func (s *dtScanner) lastLine() []byte {
	t := bytes.TrimSpace(s.tail)
	if i := bytes.LastIndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	return bytes.TrimSpace(t)
}

// within reports whether any literal falls in (lo, hi]. Checking bucket max > lo and min <= hi is exact
// for windows of at least an hour: the first bucket is then entirely <= hi and the last entirely > lo.
func (s *dtScanner) within(lo, hi time.Time) bool {
	l, h := lo.UTC().Format(dtLayout), hi.UTC().Format(dtLayout)
	for t := lo.UTC().Truncate(time.Hour); !t.After(hi); t = t.Add(time.Hour) {
		b := s.hours[t.Format(dtLayout)[:13]]
		if b != nil && b.max > l && b.min <= h {
			return true
		}
	}
	return false
}
