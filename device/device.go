// Package device talks to an iPhone over USB (via usbmuxd) using go-ios, and
// turns the many low-level failure modes into a small set of friendly states.
package device

import (
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/afc"
	"github.com/danielpaulus/go-ios/ios/house_arrest"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
)

// BookPlayerName is matched against the app's display/bundle name to find
// BookPlayer on the phone. See FindBookPlayer.
const BookPlayerName = "BookPlayer"

// BookPlayerBundleID, when non-empty, is used directly instead of looking the
// app up by name. It can be overridden with the -bundle flag.
var BookPlayerBundleID = ""

// DocumentsDir is where house_arrest exposes the app's Documents folder.
const DocumentsDir = "/Documents"

// State is the coarse connection state shown to the user.
type State string

const (
	StateNoService    State = "no_service"    // usbmuxd / Apple drivers missing
	StateNoDevice     State = "no_device"     // nothing plugged in
	StateNotTrusted   State = "not_trusted"   // needs "Trust This Computer"
	StateLocked       State = "locked"        // needs unlocking
	StateNoBookPlayer State = "no_bookplayer" // app not installed
	StateReady        State = "ready"
	StateError        State = "error"
)

// Status is a user-facing snapshot of the phone connection.
type Status struct {
	State   State  `json:"state"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Device  string `json:"device,omitempty"`
}

// Error is an error with a friendly State attached.
type Error struct {
	State State
	Err   error
}

func (e *Error) Error() string { return friendlyMessage(e.State) + " (" + e.Err.Error() + ")" }
func (e *Error) Unwrap() error { return e.Err }

func friendlyMessage(s State) string {
	switch s {
	case StateNoService:
		if runtime.GOOS == "windows" {
			return "Apple's iPhone software isn't installed. Install \"Apple Devices\" from the Microsoft Store (or iTunes), then try again."
		}
		return "Can't reach the iPhone service on this computer. Try restarting the computer."
	case StateNoDevice:
		return "Plug in your iPhone"
	case StateNotTrusted:
		return "Tap Trust on your phone"
	case StateLocked:
		return "Unlock your iPhone"
	case StateNoBookPlayer:
		return "BookPlayer isn't on this iPhone. Install it from the App Store, then try again."
	case StateReady:
		return "iPhone connected"
	}
	return "Something went wrong talking to the iPhone. Unplug it and plug it back in."
}

// classify maps a raw go-ios error to a State.
func classify(err error) State {
	var de *Error
	if errors.As(err, &de) {
		return de.State
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "usbmuxd socket") || strings.Contains(msg, "USBMuxConnection failed") ||
		strings.Contains(msg, "Could not create usbmuxConnection") || strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "No connection could be made"):
		return StateNoService
	case strings.Contains(msg, "PasswordProtected") || strings.Contains(msg, "DeviceLocked"):
		return StateLocked
	case strings.Contains(msg, "PairRecord") || strings.Contains(msg, "is the device paired") ||
		strings.Contains(msg, "InvalidHostID") || strings.Contains(msg, "PairingDialog") ||
		strings.Contains(msg, "UserDeniedPairing") || strings.Contains(msg, "NotPaired"):
		return StateNotTrusted
	case strings.Contains(msg, "no iOS devices") || strings.Contains(msg, "not found. Is it attached"):
		return StateNoDevice
	case strings.Contains(msg, "InstallationLookupFailed") || strings.Contains(msg, "ApplicationLookupFailed"):
		return StateNoBookPlayer
	}
	return StateError
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return err
	}
	return &Error{State: classify(err), Err: err}
}

// StatusFromError builds a user-facing Status from any error.
func StatusFromError(err error) Status {
	s := classify(err)
	detail := err.Error()
	var de *Error
	if errors.As(err, &de) {
		detail = de.Err.Error()
	}
	return Status{State: s, Message: friendlyMessage(s), Detail: detail}
}

// First returns the first USB-connected device.
func First() (ios.DeviceEntry, error) {
	list, err := ios.ListDevices()
	if err != nil {
		return ios.DeviceEntry{}, &Error{State: StateNoService, Err: err}
	}
	for _, d := range list.DeviceList {
		if d.Properties.ConnectionType == "" || strings.EqualFold(d.Properties.ConnectionType, "USB") {
			return d, nil
		}
	}
	return ios.DeviceEntry{}, &Error{State: StateNoDevice, Err: errors.New("no iOS devices are attached to this host")}
}

var (
	pairMu       sync.Mutex
	lastPairTry  = map[string]time.Time{}
	pairInterval = 5 * time.Second
)

// ensureTrusted checks for a pair record and a working lockdown session. If
// the phone isn't paired yet it asks the phone to show the "Trust This
// Computer?" dialog (rate-limited so we don't spam it while the user decides).
func ensureTrusted(d ios.DeviceEntry) error {
	udid := d.Properties.SerialNumber
	lockdown, err := ios.ConnectLockdownWithSession(d)
	if err == nil {
		lockdown.Close()
		return nil
	}
	state := classify(err)
	if state != StateNotTrusted {
		return wrap(err)
	}

	pairMu.Lock()
	defer pairMu.Unlock()
	if time.Since(lastPairTry[udid]) < pairInterval {
		return &Error{State: StateNotTrusted, Err: err}
	}
	lastPairTry[udid] = time.Now()
	perr := ios.Pair(d)
	if perr == nil {
		return nil
	}
	if s := classify(perr); s == StateLocked {
		return &Error{State: StateLocked, Err: perr}
	}
	return &Error{State: StateNotTrusted, Err: perr}
}

// Ping checks that the device is still reachable and trusted.
func Ping(d ios.DeviceEntry) error { return ensureTrusted(d) }

// DeviceName returns the phone's user-visible name ("Mom's iPhone"), or "".
func DeviceName(d ios.DeviceEntry) string {
	lockdown, err := ios.ConnectLockdownWithSession(d)
	if err != nil {
		return ""
	}
	defer lockdown.Close()
	v, err := lockdown.GetValue("DeviceName")
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// App is a minimal description of an installed app.
type App struct {
	BundleID    string
	Name        string
	Version     string
	FileSharing bool
}

// ListApps returns all user-installed apps.
func ListApps(d ios.DeviceEntry) ([]App, error) {
	if err := ensureTrusted(d); err != nil {
		return nil, err
	}
	conn, err := installationproxy.New(d)
	if err != nil {
		return nil, wrap(err)
	}
	defer conn.Close()
	infos, err := conn.BrowseUserApps()
	if err != nil {
		return nil, wrap(err)
	}
	apps := make([]App, 0, len(infos))
	for _, a := range infos {
		name, _ := a[installationproxy.CFBundleDisplayName].(string)
		if name == "" {
			name = a.CFBundleName()
		}
		apps = append(apps, App{
			BundleID:    a.CFBundleIdentifier(),
			Name:        name,
			Version:     a.CFBundleShortVersionString(),
			FileSharing: a.UIFileSharingEnabled(),
		})
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return apps, nil
}

// FindBookPlayer returns BookPlayer's bundle ID on the given device.
func FindBookPlayer(d ios.DeviceEntry) (string, error) {
	apps, err := ListApps(d)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if BookPlayerBundleID != "" && a.BundleID == BookPlayerBundleID {
			return a.BundleID, nil
		}
	}
	if BookPlayerBundleID == "" {
		for _, a := range apps {
			if strings.EqualFold(a.Name, BookPlayerName) {
				return a.BundleID, nil
			}
		}
	}
	return "", &Error{State: StateNoBookPlayer, Err: errors.New("BookPlayer is not installed")}
}

// Check returns the current connection status, doing the full chain of
// checks: service, device, trust, BookPlayer installed.
func Check() (Status, ios.DeviceEntry, string) {
	d, err := First()
	if err != nil {
		return StatusFromError(err), d, ""
	}
	bundle, err := FindBookPlayer(d)
	if err != nil {
		return StatusFromError(err), d, ""
	}
	st := Status{State: StateReady, Message: friendlyMessage(StateReady), Device: DeviceName(d)}
	return st, d, bundle
}

// Session is an open connection to BookPlayer's Documents folder.
type Session struct {
	UDID string
	afc  *afc.Client
	dirs map[string]bool
}

// Open opens BookPlayer's Documents folder on the device.
func Open(d ios.DeviceEntry, bundleID string) (*Session, error) {
	if err := ensureTrusted(d); err != nil {
		return nil, err
	}
	c, err := house_arrest.New(d, bundleID)
	if err != nil {
		return nil, wrap(err)
	}
	return &Session{UDID: d.Properties.SerialNumber, afc: c, dirs: map[string]bool{}}, nil
}

// Close closes the session.
func (s *Session) Close() error { return s.afc.Close() }

// mkdirAll creates every directory in p (an absolute device path).
func (s *Session) mkdirAll(p string) error {
	if p == "/" || p == "." || p == DocumentsDir || s.dirs[p] {
		return nil
	}
	if err := s.mkdirAll(path.Dir(p)); err != nil {
		return err
	}
	if fi, err := s.afc.Stat(p); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a folder", p)
		}
	} else if err := s.afc.MkDir(p); err != nil {
		return err
	}
	s.dirs[p] = true
	return nil
}

// CleanRel sanitizes a user-supplied relative path ("Book/01.mp3") so it
// can't escape Documents. Returns "" if nothing usable is left.
func CleanRel(rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	var parts []string
	for _, p := range strings.Split(rel, "/") {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/")
}

// Write copies r into Documents/rel, creating folders as needed.
func (s *Session) Write(rel string, r io.Reader) error {
	rel = CleanRel(rel)
	if rel == "" {
		return errors.New("empty file name")
	}
	dst := path.Join(DocumentsDir, rel)
	if err := s.mkdirAll(path.Dir(dst)); err != nil {
		return wrap(err)
	}
	f, err := s.afc.Open(dst, afc.WRITE_ONLY_CREATE_TRUNC)
	if err != nil {
		return wrap(err)
	}
	// Each AFC write is a round trip, so fill 1 MiB chunks rather than passing
	// through whatever small reads the source (e.g. an HTTP body) yields.
	buf := make([]byte, 1<<20)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return wrap(err)
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	return wrap(f.Close())
}

// List lists a folder relative to Documents (for debugging).
func (s *Session) List(rel string) ([]string, error) {
	return s.afc.List(path.Join(DocumentsDir, CleanRel(rel)))
}

// Allowed reports whether a file should be sent to BookPlayer.
func Allowed(name string) bool {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	if strings.HasPrefix(base, ".") {
		return false
	}
	switch strings.ToLower(path.Ext(base)) {
	case ".mp3", ".m4a", ".m4b", ".zip":
		return true
	}
	return false
}
