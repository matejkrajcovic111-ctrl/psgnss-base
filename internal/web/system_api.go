package web

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"time"
)

// Power control for the host the base runs on.
//
// Three deliberate constraints: the action is chosen from a fixed table rather
// than passed through to a shell, each action requires its own typed
// confirmation so a stray click cannot take the base off the air, and every
// attempt is logged with the administrator who made it. The response is sent
// before the command runs, because a reboot ends the connection that would
// otherwise carry it.
var powerActions = map[string]struct {
	confirm string
	args    []string
	log     string
}{
	"reboot":   {"REBOOT", []string{"reboot"}, "host reboot requested from the web UI"},
	"shutdown": {"SHUTDOWN", []string{"poweroff"}, "host shutdown requested from the web UI"},
}

func (s *Server) handleSystemPower(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action  string `json:"action"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	spec, ok := powerActions[in.Action]
	if !ok {
		writeErr(w, http.StatusBadRequest, "action must be reboot or shutdown")
		return
	}
	if in.Confirm != spec.confirm {
		writeErr(w, http.StatusBadRequest, "confirmation must be "+spec.confirm)
		return
	}
	path, err := exec.LookPath("systemctl")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "systemctl is not available on this host")
		return
	}
	s.Log.Warn(spec.log, "by", adminOf(r))
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "action": in.Action})
	go func() {
		// Long enough for the response to reach the browser, short enough that
		// an operator does not press the button twice.
		time.Sleep(time.Second)
		if out, err := exec.Command(path, spec.args...).CombinedOutput(); err != nil {
			s.Log.Error("power action failed", "action", in.Action, "err", err, "output", string(out))
		}
	}()
}
