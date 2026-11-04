package importer

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type counter struct {
	r io.Reader
	n atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// bar draws a single-line progress bar; it also implements io.Writer so
// mysql's stderr can be interleaved without garbling the line.
type bar struct {
	mu    sync.Mutex
	w     io.Writer
	n     *counter
	size  int64
	start time.Time
	label string
	shown bool
}

const barWidth = 30

func (b *bar) draw() {
	b.mu.Lock()
	defer b.mu.Unlock()

	done := b.n.n.Load()
	pct := 1.0
	if b.size > 0 {
		pct = min(float64(done)/float64(b.size), 1)
	}

	rate := float64(done) / time.Since(b.start).Seconds()
	eta := "--"
	if rate > 0 && done < b.size {
		eta = time.Duration(float64(b.size-done) / rate * float64(time.Second)).Round(time.Second).String()
	}

	fill := int(pct * barWidth)
	fmt.Fprintf(b.w, "\r\033[K%s[%s%s] %3.0f%%  %s / %s  %s/s  ETA %s", b.label,
		strings.Repeat("=", fill), strings.Repeat(" ", barWidth-fill), pct*100,
		HumanBytes(done), HumanBytes(b.size), HumanBytes(int64(rate)), eta)
	b.shown = true
}

// clear erases the bar; caller holds mu.
func (b *bar) clear() {
	if b.shown {
		io.WriteString(b.w, "\r\033[K")
		b.shown = false
	}
}

func (b *bar) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.clear()
	return b.w.Write(p)
}

// start200ms redraws until the returned stop func is called; stop erases the bar.
func (b *bar) start200ms() (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)

		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()

		for {
			select {
			case <-quit:
				b.mu.Lock()
				b.clear()
				b.mu.Unlock()
				return
			case <-t.C:
				b.draw()
			}
		}
	}()

	return func() {
		close(quit)
		<-done
	}
}

// HumanBytes formats n using binary units, e.g. 1536 -> "1.5 KiB".
func HumanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}

	d, e := int64(u), 0
	for m := n / u; m >= u; m /= u {
		d *= u
		e++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(d), "KMGTPE"[e])
}
