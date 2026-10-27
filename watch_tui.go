package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

type watchFilter int

const (
	filterAll watchFilter = iota
	filterActive
	filterJobs
	filterLocal
)

var watchFilterNames = [...]string{"all", "active", "jobs", "local"}

type watchView struct {
	offset  int
	compact bool
	filter  watchFilter
}

func watchCommand(args []string) error {
	f := flag.NewFlagSet("watch", flag.ContinueOnError)
	interval := f.Duration("interval", 2*time.Second, "Refresh interval (at least 500ms)")
	plain := f.Bool("plain", false, "Append plain frames without terminal control codes")
	once := f.Bool("once", false, "Print one plain frame and exit")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *interval < 500*time.Millisecond {
		return errors.New("usage: agent-bridge watch [--interval 2s] [--plain] [--once]")
	}
	previous := map[string]TokenUsage{}
	sample := func() ([]string, string, error) {
		s, err := readLatest(10 * time.Second)
		origin := "daemon"
		if err != nil {
			origin = "direct"
			s, err = collectSnapshot()
			if err != nil {
				return nil, "", err
			}
		}
		return renderWatchFrame(s, origin, previous), origin, nil
	}
	if *once || *plain || !isTerminal(int(os.Stdout.Fd())) || !isTerminal(int(os.Stdin.Fd())) {
		for {
			lines, _, err := sample()
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintln(os.Stdout, plainText(strings.Join(lines, "\n"))); err != nil {
				return err
			}
			if *once {
				return nil
			}
			time.Sleep(*interval)
		}
	}
	return runWatchTerminal(*interval, sample)
}

func isTerminal(fd int) bool { _, err := getTermios(fd); return err == nil }
func getTermios(fd int) (syscall.Termios, error) {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return t, errnoErr(errno)
}
func setTermios(fd int, t syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t)))
	return errnoErr(errno)
}
func errnoErr(e syscall.Errno) error {
	if e != 0 {
		return e
	}
	return nil
}
func rawTerminal(fd int) (func(), error) {
	old, err := getTermios(fd)
	if err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Cflag |= syscall.CS8
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := setTermios(fd, raw); err != nil {
		return nil, err
	}
	return func() { _ = setTermios(fd, old) }, nil
}
func terminalSize(fd int) (int, int) {
	var size struct{ row, col, x, y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if e != 0 || size.col == 0 || size.row == 0 {
		return 80, 24
	}
	return int(size.col), int(size.row)
}
func runWatchTerminal(interval time.Duration, sample func() ([]string, string, error)) error {
	restore, err := rawTerminal(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer restore()
	if _, err = io.WriteString(os.Stdout, "\x1b[?1049h\x1b[?25l"); err != nil {
		return err
	}
	defer io.WriteString(os.Stdout, "\x1b[0m\x1b[?25h\x1b[?1049l")
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGWINCH)
	defer signal.Stop(signals)
	input := make(chan byte, 64)
	go func() {
		var b [1]byte
		for {
			n, e := os.Stdin.Read(b[:])
			if e != nil {
				return
			}
			if n == 1 {
				input <- b[0]
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lines, origin, err := sample()
	if err != nil {
		return err
	}
	view := watchView{}
	decoder := keyDecoder{}
	draw := func() error {
		w, h := terminalSize(int(os.Stdout.Fd()))
		frame := watchScreen(lines, w, h, &view, origin, time.Now())
		_, err := io.WriteString(os.Stdout, frame)
		return err
	}
	if err := draw(); err != nil {
		return err
	}
	for {
		select {
		case sig := <-signals:
			if sig != syscall.SIGWINCH {
				return nil
			}
			if err := draw(); err != nil {
				return err
			}
		case b := <-input:
			key := decoder.feed(b)
			if key == "" {
				continue
			}
			if key == "quit" {
				return nil
			}
			if applyWatchKey(&view, key, len(selectWatchLines(lines, view.filter, view.compact)), terminalHeight()) {
				if err := draw(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			lines, origin, err = sample()
			if err != nil {
				return err
			}
			if err := draw(); err != nil {
				return err
			}
		}
	}
}
func terminalHeight() int { _, h := terminalSize(int(os.Stdout.Fd())); return h }

// ANSI controls are retained in the interactive frame and removed from plain output.
func plainText(s string) string {
	parts := strings.Split(s, "\n")
	for i := range parts {
		parts[i] = stripANSI(parts[i])
	}
	return strings.Join(parts, "\n")
}
func stripANSI(s string) string { return truncateANSI(s, -1, false) }

func selectWatchLines(lines []string, filter watchFilter, compact bool) []string {
	if filter == filterAll && !compact {
		return lines
	}
	var out []string
	section := "header"
	for i, line := range lines {
		clean := stripANSI(line)
		switch clean {
		case "Bridge jobs:":
			section = "jobs"
		case "Steinberger:":
			section = "steinberger"
		case "VRAM:":
			section = "vram"
		}
		if i == 0 {
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(clean, "***") {
			out = append(out, line)
			continue
		}
		switch filter {
		case filterJobs:
			if section != "jobs" {
				continue
			}
		case filterLocal:
			if section == "jobs" || section == "steinberger" {
				continue
			}
			if section == "header" && !strings.HasPrefix(clean, "local") {
				continue
			}
		case filterActive:
			if section == "header" && (strings.HasPrefix(clean, "No fresh") || strings.HasPrefix(clean, "  usage") || strings.Contains(clean, " idle ") || strings.Contains(clean, " ready ") || strings.Contains(clean, " unverified ") || strings.Contains(clean, " loaded ")) {
				continue
			}
			if section == "jobs" && (strings.Contains(clean, " completed ") || strings.Contains(clean, " failed ") || strings.Contains(clean, " cancelled ") || strings.Contains(clean, " stopped ") || strings.Contains(clean, " lost ") || strings.Contains(clean, "  none in")) {
				continue
			}
		}
		if compact {
			if section != "header" && section != "jobs" {
				continue
			}
			if clean == "" || strings.HasPrefix(clean, "  usage") || strings.HasPrefix(clean, "  input") {
				continue
			}
			if section == "header" && !(strings.HasPrefix(clean, "claude ") || strings.HasPrefix(clean, "codex ") || strings.HasPrefix(clean, "local ")) {
				continue
			}
			if section == "jobs" && (clean == "Bridge jobs:" || strings.Contains(clean, "  none in")) {
				continue
			}
			if strings.Contains(clean, " idle ") || strings.Contains(clean, " ready ") || strings.Contains(clean, " unverified ") || strings.Contains(clean, " loaded ") || strings.Contains(clean, " completed ") || strings.Contains(clean, " failed ") || strings.Contains(clean, " cancelled ") || strings.Contains(clean, " stopped ") || strings.Contains(clean, " lost ") {
				continue
			}
		}
		out = append(out, line)
	}
	if len(out) == 1 {
		out = append(out, "No matching activity.")
	}
	return out
}

func maxOffset(n, height int) int {
	visible := height - 1
	if visible < 1 {
		visible = 1
	}
	if n > visible {
		return n - visible
	}
	return 0
}
func applyWatchKey(v *watchView, key string, n, height int) bool {
	old := *v
	page := height - 2
	if page < 1 {
		page = 1
	}
	switch key {
	case "down":
		v.offset++
	case "up":
		v.offset--
	case "pageDown":
		v.offset += page
	case "pageUp":
		v.offset -= page
	case "top":
		v.offset = 0
	case "bottom":
		v.offset = maxOffset(n, height)
	case "compact":
		v.compact = !v.compact
		v.offset = 0
	case "filter":
		v.filter = (v.filter + 1) % watchFilter(len(watchFilterNames))
		v.offset = 0
	}
	if v.offset < 0 {
		v.offset = 0
	}
	if v.offset > maxOffset(n, height) {
		v.offset = maxOffset(n, height)
	}
	return *v != old
}
func watchScreen(all []string, w, h int, v *watchView, origin string, now time.Time) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	lines := selectWatchLines(all, v.filter, v.compact)
	if v.offset > maxOffset(len(lines), h) {
		v.offset = maxOffset(len(lines), h)
	}
	visible := h - 1
	if visible < 0 {
		visible = 0
	}
	end := v.offset + visible
	if end > len(lines) {
		end = len(lines)
	}
	first, last := 0, 0
	if len(lines) > 0 && visible > 0 {
		first = v.offset + 1
		last = end
	}
	status := fmt.Sprintf("%s UTC %s lines %d–%d of %d %s | q c f | j/k ↑/↓ PgUp/Dn space g/G", now.UTC().Format("15:04:05"), origin, first, last, len(lines), watchFilterNames[v.filter])
	if v.compact {
		status += " compact"
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	for row := 0; row < h; row++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[K", row+1)
		line := ""
		if row == h-1 {
			line = status
		} else if v.offset+row < end {
			line = lines[v.offset+row]
		}
		// Reserve the final column to avoid an automatic wrap at the bottom edge.
		width := w - 1
		if width < 0 {
			width = 0
		}
		b.WriteString(truncateANSI(line, width, true))
		b.WriteString("\x1b[0m")
	}
	b.WriteString("\x1b[J")
	return b.String()
}

func truncateANSI(s string, width int, keepANSI bool) string {
	var b strings.Builder
	cols := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			start := i
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) {
					c := s[i]
					i++
					if c >= 0x40 && c <= 0x7e {
						break
					}
				}
				if keepANSI {
					b.WriteString(s[start:i])
				}
				continue
			}
			if i < len(s) && s[i] == ']' {
				i++
				for i < len(s) {
					if s[i] == 7 {
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
				continue
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r == '\n' || r == '\r' || r == '\t' || r < 32 {
			if r == '\t' {
				r = ' '
			} else {
				continue
			}
		}
		cw := 1
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			cw = 0
		} else if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff) {
			cw = 2
		}
		if width >= 0 && cols+cw > width {
			break
		}
		b.WriteRune(r)
		cols += cw
	}
	return b.String()
}

type keyDecoder struct {
	escape int
	digits string
}

func (d *keyDecoder) feed(b byte) string {
	if d.escape == 0 {
		if b == 27 {
			d.escape = 1
			return ""
		}
		switch b {
		case 'j':
			return "down"
		case 'k':
			return "up"
		case ' ':
			return "pageDown"
		case 'g':
			return "top"
		case 'G':
			return "bottom"
		case 'c':
			return "compact"
		case 'f':
			return "filter"
		case 'q', 3:
			return "quit"
		}
		return ""
	}
	if d.escape == 1 {
		if b == '[' || b == 'O' {
			d.escape = 2
			d.digits = ""
			return ""
		}
		d.escape = 0
		return d.feed(b)
	}
	if b >= '0' && b <= '9' {
		d.digits += string(b)
		return ""
	}
	if b == ';' {
		return ""
	}
	d.escape = 0
	switch b {
	case 'A':
		return "up"
	case 'B':
		return "down"
	case '~':
		if d.digits == "5" {
			return "pageUp"
		}
		if d.digits == "6" {
			return "pageDown"
		}
	}
	return ""
}
