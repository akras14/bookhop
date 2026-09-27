package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios"

	"bookhop/device"
)

//go:embed web
var webFS embed.FS

const (
	defaultPort  = 47831
	appID        = "bookhop"
	pollInterval = 2 * time.Second
	// How long to wait after the last browser tab disconnects before exiting.
	// Long enough to survive a page reload.
	idleExit = 5 * time.Second
)

type server struct {
	mu      sync.Mutex
	status  device.Status
	dev     ios.DeviceEntry
	bundle  string // BookPlayer bundle ID for dev, once found
	// lastUpload is when an upload last started or finished. It's zero when idle.
	// A batch counts as busy for a while after, so a crashed tab can't wedge us.
	lastUpload time.Time
	uploading  bool // a file is being written right now
	session *device.Session

	uploadMu sync.Mutex // serializes uploads

	clients  int
	changed  chan struct{} // closed and replaced whenever status changes
	lastSeen time.Time
	quit     chan struct{}
}

func runUI(port int, openBrowser bool) error {
	if port == 0 {
		port = defaultPort
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	url := "http://" + addr + "/"

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Already running? Just point the browser at the existing instance.
		if isOurs(url) {
			fmt.Println("Send Books to iPhone is already running at", url)
			if openBrowser {
				openURL(url)
			}
			return nil
		}
		// Port taken by something else: use any free port.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		url = "http://" + ln.Addr().String() + "/"
	}

	s := &server{
		status:  device.Status{State: device.StateNoDevice, Message: "Looking for your iPhone…"},
		changed: make(chan struct{}),
		quit:    make(chan struct{}),
	}

	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(web)))
	mux.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, appID) })
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/done", s.handleDone)

	srv := &http.Server{Handler: localOnly(mux)}
	go s.poll()
	go s.watchIdle()

	fmt.Println("Send Books to iPhone is running at", url)
	fmt.Println("Keep this window open while sending books. Closing the browser tab stops it.")
	if openBrowser {
		openURL(url)
	}

	go func() {
		<-s.quit
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localOnly rejects requests whose Host isn't loopback, which blocks DNS
// rebinding attacks from web pages trying to reach this server.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func isOurs(url string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(url + "api/hello")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var b [16]byte
	n, _ := resp.Body.Read(b[:])
	return string(b[:n]) == appID
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("Open this address in your browser:", url)
	}
}

// setStatus updates the status and wakes every event stream if it changed.
func (s *server) setStatus(st device.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st == s.status {
		return
	}
	s.status = st
	close(s.changed)
	s.changed = make(chan struct{})
}

// poll keeps the device status fresh. It skips checks during uploads, and
// only re-lists apps (slow) when a new device shows up or BookPlayer was
// missing last time.
func (s *server) poll() {
	for {
		if !s.busy() {
			s.check()
		}
		select {
		case <-time.After(pollInterval):
		case <-s.quit:
			return
		}
	}
}

func (s *server) check() {
	d, err := device.First()
	if err != nil {
		s.forget()
		s.setStatus(device.StatusFromError(err))
		return
	}
	s.mu.Lock()
	sameDevice := s.bundle != "" && s.dev.Properties.SerialNumber == d.Properties.SerialNumber
	name := s.status.Device
	s.mu.Unlock()

	if sameDevice {
		// Cheap check: is the phone still reachable and trusted?
		if err := device.Ping(d); err != nil {
			s.forget()
			s.setStatus(device.StatusFromError(err))
			return
		}
		s.mu.Lock()
		s.dev = d // DeviceID changes on replug
		s.mu.Unlock()
		s.setStatus(device.Status{State: device.StateReady, Message: "iPhone connected", Device: name})
		return
	}

	bundle, err := device.FindBookPlayer(d)
	if err != nil {
		s.forget()
		s.setStatus(device.StatusFromError(err))
		return
	}
	name = device.DeviceName(d)
	s.mu.Lock()
	s.dev, s.bundle = d, bundle
	s.mu.Unlock()
	s.setStatus(device.Status{State: device.StateReady, Message: "iPhone connected", Device: name})
}

// forget drops the cached device and any open session.
func (s *server) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundle = ""
	if s.session != nil && !s.busyLocked() {
		s.session.Close()
		s.session = nil
	}
}

// handleEvents streams status updates to the page (Server-Sent Events). The
// open stream doubles as the heartbeat: when no page is connected, we exit.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	s.mu.Lock()
	s.clients++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.clients--
		s.lastSeen = time.Now()
		s.mu.Unlock()
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		s.mu.Lock()
		st, changed := s.status, s.changed
		s.mu.Unlock()
		b, _ := json.Marshal(st)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		flusher.Flush()
	wait:
		select {
		case <-changed:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			goto wait
		case <-r.Context().Done():
			return
		case <-s.quit:
			return
		}
	}
}

// watchIdle exits the app once no browser tab has been connected for idleExit.
// Before the first tab connects we wait much longer, in case the browser is
// slow to start.
func (s *server) watchIdle() {
	start := time.Now()
	for range time.Tick(time.Second) {
		s.mu.Lock()
		clients, last := s.clients, s.lastSeen
		s.mu.Unlock()
		if clients > 0 || s.busy() {
			continue
		}
		if (last.IsZero() && time.Since(start) > 10*time.Minute) ||
			(!last.IsZero() && time.Since(last) > idleExit) {
			fmt.Println("Browser closed, exiting.")
			close(s.quit)
			return
		}
	}
}

type apiError struct {
	Error  string `json:"error"`
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func writeErr(w http.ResponseWriter, code int, err error) {
	st := device.StatusFromError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(apiError{Error: st.Message, State: string(st.State), Detail: st.Detail})
}

// handleUpload receives one file (PUT /api/upload?path=Folder/file.mp3) and
// streams it straight into BookPlayer's Documents folder.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := device.CleanRel(r.URL.Query().Get("path"))
	if rel == "" || !device.Allowed(rel) {
		// Not an audiobook: ignore silently.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	s.setUploading(true)
	defer s.setUploading(false)

	sess, err := s.openSession()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if err := sess.Write(rel, r.Body); err != nil {
		// Drop the session so the next file retries with a fresh connection.
		s.mu.Lock()
		if s.session == sess {
			s.session.Close()
			s.session = nil
		}
		s.mu.Unlock()
		s.check()
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) openSession() (*device.Session, error) {
	s.touch()
	s.mu.Lock()
	if s.session != nil {
		sess := s.session
		s.mu.Unlock()
		return sess, nil
	}
	d, bundle := s.dev, s.bundle
	s.mu.Unlock()

	if bundle == "" {
		st, dd, b := device.Check()
		if st.State != device.StateReady {
			return nil, &device.Error{State: st.State, Err: errors.New(st.Detail)}
		}
		d, bundle = dd, b
	}
	sess, err := device.Open(d, bundle)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()
	return sess, nil
}

// handleDone ends an upload batch and closes the device connection.
func (s *server) handleDone(w http.ResponseWriter, r *http.Request) {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	s.mu.Lock()
	if s.session != nil {
		s.session.Close()
		s.session = nil
	}
	s.lastUpload = time.Time{}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

const busyTimeout = 30 * time.Second

func (s *server) touch() {
	s.mu.Lock()
	s.lastUpload = time.Now()
	s.mu.Unlock()
}

func (s *server) setUploading(on bool) {
	s.mu.Lock()
	s.uploading = on
	s.lastUpload = time.Now()
	s.mu.Unlock()
}

func (s *server) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busyLocked()
}

func (s *server) busyLocked() bool {
	return s.uploading || (!s.lastUpload.IsZero() && time.Since(s.lastUpload) < busyTimeout)
}
