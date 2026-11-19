//go:build windows

package watch

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/YoanWai/agent-manager/internal/tmux"
)

// psmuxPane creates a managed session whose PowerShell pane printed ready.
func psmuxPane(t *testing.T, driver *tmux.Driver, prefix string) string {
	t.Helper()
	id := prefix + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	if err := driver.Create(id, os.TempDir(), "Write-Host ready", nil, 100, 30); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { driver.Kill(id) })
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		pane, err := driver.CapturePane(id)
		if err == nil && strings.Contains(pane, "ready") {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never printed ready: %q", id, pane)
		}
	}
}

// framesOf collects the frames pushed for id.
func framesOf(id string) (chan Frame, func(any)) {
	frames := make(chan Frame, 1024)
	return frames, func(msg any) {
		if frame, ok := msg.(Frame); ok && frame.SessionID == id {
			select {
			case frames <- frame:
			default:
			}
		}
	}
}

func waitFrame(t *testing.T, frames <-chan Frame, contains string, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := time.After(within)
	for {
		select {
		case frame := <-frames:
			if strings.Contains(frame.Preview, contains) {
				return time.Since(start)
			}
		case <-deadline:
			t.Fatalf("no frame showing %q within %v", contains, within)
			return 0
		}
	}
}

func waitServing(t *testing.T, watch *Watcher, id string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !watch.Serving(id); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("watcher never served %s", id)
		}
	}
}

// The focused psmux pane is served by its control client: a frame is pushed
// when the pane paints rather than found by a forked look, and a write rides
// the client only where its server answers writes.
func TestFocusWatchServesAPsmuxPaneOverItsControlClient(t *testing.T) {
	driver := requireDriver(t)
	id := psmuxPane(t, driver, "psctl")
	frames, send := framesOf(id)
	watch := newTestWatcher(driver, send)
	t.Cleanup(watch.Close)
	watch.SetFocus(id)
	waitServing(t, watch, id)
	watch.mu.Lock()
	control, polled := watch.control, watch.polled
	watch.mu.Unlock()
	if control == nil || polled {
		t.Fatalf("watcher serves %s by polling, want its control client", id)
	}
	command := tmux.SendKeysHex(tmux.PaneTarget(id), []byte("Write-Output ('pushed'+'frame')\r"))
	// A forked key lands a fork after the frames streaming meanwhile, so
	// it is counted and no frame reads as caught up until it lands.
	counted := watch.CountWrite(id)
	if counted == control.CarriesWrites() {
		t.Fatalf("CountWrite = %v with writes over the client %v, want only forked writes counted", counted, control.CarriesWrites())
	}
	if counted && watch.CaughtUp(0) {
		t.Fatal("a frame from before the counted write reads as caught up")
	}
	available, err := watch.Forward(command)
	if available != control.CarriesWrites() {
		t.Fatalf("Forward available = %v, want the probe's answer %v", available, control.CarriesWrites())
	}
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if !available {
		if err := driver.SendRaw(command); err != nil {
			t.Fatalf("forked send-keys: %v", err)
		}
	}
	watch.InputSent(id)
	if counted {
		watch.WriteLanded(id)
	}
	start := time.Now()
	for deadline := time.After(5 * time.Second); ; {
		select {
		case frame := <-frames:
			if !strings.Contains(frame.Preview, "pushedframe") {
				continue
			}
			if !watch.CaughtUp(frame.Writes) {
				t.Fatalf("the frame showing the echo is not caught up: Writes = %d", frame.Writes)
			}
			t.Logf("writes over the client: %v; typed command to its output on screen: %v", available, time.Since(start))
			return
		case <-deadline:
			t.Fatal("no frame showing the echo within 5s")
		}
	}
}

// Moving focus between psmux panes opens the next pane's client at once.
// psmux 3.4.0's client lingers 2 s after its stdin closes, and the next
// attach waited on it under the gate until Close ended it outright; the
// pane left behind keeps no client.
func TestFocusWatchMovesBetweenPsmuxPanes(t *testing.T) {
	driver := requireDriver(t)
	first, second := psmuxPane(t, driver, "psfirst"), psmuxPane(t, driver, "pssecond")
	frames := map[string]chan Frame{}
	sends := map[string]func(any){}
	for _, id := range []string{first, second} {
		frames[id], sends[id] = framesOf(id)
	}
	watch := newTestWatcher(driver, func(msg any) {
		for _, send := range sends {
			send(msg)
		}
	})
	t.Cleanup(watch.Close)
	var moves []time.Duration
	for _, id := range []string{first, second, first, second} {
		start := time.Now()
		watch.SetFocus(id)
		waitServing(t, watch, id)
		waitFrame(t, frames[id], "ready", 10*time.Second)
		moves = append(moves, time.Since(start))
	}
	t.Logf("focus to first pushed frame: %v", moves)
	for _, move := range moves[1:] {
		if move > time.Second {
			t.Fatalf("a focus move took %v to serve the pane, want under 1s: %v", move, moves)
		}
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		attached, err := driver.Display(first, "#{session_attached}")
		if err == nil && strings.TrimSpace(attached) == "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane focus left still has %q clients (%v)", strings.TrimSpace(attached), err)
		}
	}
}
