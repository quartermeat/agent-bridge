package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const idleTimeout = 5 * time.Minute
const eventTailBytes int64 = 1024 * 1024

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type job struct {
	ID       string `json:"id"`
	Launcher string `json:"launcher"`
	Status   string `json:"status"`
	Agent    string `json:"agent"`
	Name     string `json:"name"`
}
type instance struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}
type viewer struct {
	id, dir, token string
	last           atomic.Int64
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 2 && args[0] == "_serve" {
		return serve(args[1])
	}
	if len(args) != 1 {
		return errors.New("usage: widget-job-view ID")
	}
	id, dir, err := target(args[0])
	if err != nil {
		return err
	}
	state := filepath.Join(filepath.Dir(filepath.Dir(dir)), "viewers")
	if err := os.MkdirAll(state, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(state, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	meta := filepath.Join(state, id+".json")
	inst, ok := readInstance(meta)
	if !ok || !healthy(inst) {
		if err := os.Remove(meta); err != nil && !os.IsNotExist(err) {
			return err
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(self, "_serve", id)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer devnull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		if err := cmd.Start(); err != nil {
			return err
		}
		_ = cmd.Process.Release()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			inst, ok = readInstance(meta)
			if ok && healthy(inst) {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if !ok || !healthy(inst) {
			return errors.New("viewer did not start")
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v/%s", inst.Port, inst.Token)
	am := os.Getenv("WIDGET_JOB_VIEW_AM")
	if am == "" {
		am = "am"
	}
	out, err := exec.Command(am, "start", "-a", "android.intent.action.VIEW", "-d", url,
		"--es", "com.android.browser.application_id", "widget-job-view."+id, "--activity-single-top").CombinedOutput()
	if err != nil {
		return fmt.Errorf("open browser: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func target(id string) (string, string, error) {
	if !validID.MatchString(id) {
		return "", "", errors.New("invalid job ID")
	}
	root := os.Getenv("AGENT_BRIDGE_STATE_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		root = filepath.Join(home, ".local", "state", "agent-bridge")
	}
	dir := filepath.Join(root, "jobs", id)
	for _, p := range []string{filepath.Join(root, "jobs"), dir, filepath.Join(dir, "job.json")} {
		st, err := os.Lstat(p)
		if err != nil {
			return "", "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("symlink in job path")
		}
	}
	b, err := readJobFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return "", "", err
	}
	var j job
	if err = json.Unmarshal(b, &j); err != nil {
		return "", "", err
	}
	if j.ID != id || j.Launcher != "direct" {
		return "", "", errors.New("job identity or launcher mismatch")
	}
	return id, dir, nil
}

func readInstance(path string) (instance, bool) {
	var v instance
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &v) != nil || v.Port < 1 || v.Port > 65535 || len(v.Token) != 64 {
		return v, false
	}
	return v, true
}
func healthy(v instance) bool {
	c := &http.Client{Timeout: 300 * time.Millisecond}
	r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/v/%s/health", v.Port, v.Token))
	if err != nil {
		return false
	}
	defer r.Body.Close()
	return r.StatusCode == 204
}

func serve(id string) error {
	id, dir, err := target(id)
	if err != nil {
		return err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	v := &viewer{id: id, dir: dir, token: hex.EncodeToString(raw[:])}
	v.touch()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	state := filepath.Join(filepath.Dir(filepath.Dir(dir)), "viewers")
	meta := filepath.Join(state, id+".json")
	b, _ := json.Marshal(instance{Port: port, Token: v.token})
	tmp := meta + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, meta); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", v.handle)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if v.expired() {
					_ = srv.Close()
					return
				}
			}
		}
	}()
	err = srv.Serve(ln)
	close(done)
	lock, e := os.OpenFile(filepath.Join(state, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e == nil {
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) == nil {
			if current, ok := readInstance(meta); ok && current.Token == v.token {
				_ = os.Remove(meta)
			}
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
		_ = lock.Close()
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (v *viewer) touch()        { v.last.Store(time.Now().UnixNano()) }
func (v *viewer) expired() bool { return time.Since(time.Unix(0, v.last.Load())) >= idleTimeout }
func (v *viewer) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	if r.Method != http.MethodGet || !validHost(r.Host) {
		http.NotFound(w, r)
		return
	}
	base := "/v/" + v.token
	if r.URL.Path != base && r.URL.Path != base+"/data" && r.URL.Path != base+"/health" {
		http.NotFound(w, r)
		return
	}
	v.touch()
	if r.URL.Path == base+"/health" {
		w.WriteHeader(204)
		return
	}
	if r.URL.Path == base+"/data" {
		name, status, output, err := v.snapshot()
		if err != nil {
			http.Error(w, "job unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Output string `json:"output"`
		}{v.id, name, status, output})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := v.id
	if b, err := readJobFile(filepath.Join(v.dir, "job.json")); err == nil {
		var j job
		if json.Unmarshal(b, &j) == nil && j.ID == v.id && j.Name != "" {
			name = j.Name
		}
	}
	_ = page.Execute(w, name)
}
func validHost(h string) bool {
	host, _, err := net.SplitHostPort(h)
	return err == nil && host == "127.0.0.1"
}

var page = template.Must(template.New("page").Parse(`<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>Job {{.}}</title><style>body{font:16px system-ui;margin:1rem;max-width:70ch;background:#111;color:#eee}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:15px/1.5 ui-monospace,monospace}</style><h1 id="heading">Job {{.}}</h1><p id="status">Loading…</p><pre id="output"></pre><script>async function update(){try{let r=await fetch(location.pathname+'/data',{cache:'no-store'});if(!r.ok)throw 0;let d=await r.json();let label='Job '+(d.name||d.id);document.title=label;document.getElementById('heading').textContent=label;document.getElementById('status').textContent=d.status;document.getElementById('output').textContent=d.output}catch{document.getElementById('status').textContent='Viewer unavailable'}}update();setInterval(update,2000)</script>`))

func (v *viewer) snapshot() (string, string, string, error) {
	_, dir, err := target(v.id)
	if err != nil || dir != v.dir {
		return "", "", "", errors.New("job changed")
	}
	b, err := readJobFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return "", "", "", err
	}
	var j job
	if err = json.Unmarshal(b, &j); err != nil {
		return "", "", "", err
	}
	var texts []string
	if f, err := safeOpen(filepath.Join(dir, "events.jsonl")); err == nil {
		defer f.Close()
		for _, line := range recentEventLines(f) {
			if t := visibleEvent(line, j.Agent); t != "" {
				texts = append(texts, t)
			}
		}
	}
	output := strings.Join(texts, "\n\n")
	if f, err := safeOpen(filepath.Join(dir, "result.txt")); err == nil {
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, 1024*1024))
		final := strings.TrimSpace(string(b))
		if final != "" && !strings.HasSuffix(output, final) {
			if output != "" {
				output += "\n\n"
			}
			output += final
		}
	}
	if len(output) > 200000 {
		cut := len(output) - 200000
		for cut < len(output) && !utf8.RuneStart(output[cut]) {
			cut++
		}
		output = "… earlier output omitted …\n\n" + output[cut:]
	}
	return j.Name, j.Status, output, nil
}
func recentEventLines(f *os.File) [][]byte {
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	start := st.Size() - eventTailBytes
	if start < 0 {
		start = 0
	}
	b := make([]byte, st.Size()-start)
	n, err := f.ReadAt(b, start)
	if err != nil && err != io.EOF {
		return nil
	}
	b = b[:n]
	if start > 0 {
		first := bytes.IndexByte(b, '\n')
		if first < 0 {
			return nil
		}
		b = b[first+1:]
	}
	last := bytes.LastIndexByte(b, '\n')
	if last < 0 {
		return nil
	}
	return bytes.Split(b[:last], []byte{'\n'})
}

func safeOpen(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("not a regular file")
	}
	return f, nil
}
func readJobFile(path string) ([]byte, error) {
	f, err := safeOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1024*1024))
}
func visibleEvent(line []byte, agent string) string {
	if agent == "claude" {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
			return ""
		}
		var a []string
		for _, c := range e.Message.Content {
			if c.Type == "text" && c.Text != "" {
				a = append(a, c.Text)
			}
		}
		return strings.Join(a, "\n")
	}
	var e struct {
		Type string `json:"type"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &e) == nil && (e.Type == "item.completed" || e.Type == "item.updated") && e.Item.Type == "agent_message" {
		return e.Item.Text
	}
	return ""
}
