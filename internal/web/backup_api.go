package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/psgnss/psgnss-base/internal/backup"
	"github.com/psgnss/psgnss-base/internal/version"
)

// Settings backup and restore.
//
// A backup is one encrypted file the operator keeps; a restore replaces the
// configured state with what is in it. Both are administrator-only, and neither
// touches telemetry, the connection log or the receiver audit trail -- see
// internal/backup for what is in scope and why the file is encrypted.
//
// Ordering matters more than it looks. A restore writes the configuration file,
// then the database, then restarts, and before any of that it writes a rollback
// snapshot of the current state next to the database, sealed with the same
// passphrase. If the database step fails the configuration is put back from the
// copy still in memory; if something subtler is wrong, the operator has a file
// to restore from rather than a story about what used to be configured.

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	blob, doc, err := s.sealCurrentSettings(in.Passphrase)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := backup.Filename(doc.Station, doc.Created)
	s.Log.Warn("settings backup downloaded", "by", adminOf(r), "file", name,
		"admins", len(doc.Admins), "ntrip_users", len(doc.Users))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Backup-Filename", name)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

// handleBackupInspect decrypts a file and reports what is in it without
// applying anything, so a restore is confirmed against the actual contents
// rather than against a filename.
func (s *Server) handleBackupInspect(w http.ResponseWriter, r *http.Request) {
	doc, err := s.decodeUpload(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc.Summary())
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if s.ConfigPath == "" {
		writeErr(w, http.StatusServiceUnavailable, "configuration is not writable in this environment")
		return
	}
	body, err := readUpload(w, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	doc, err := backup.Open(body.blob, body.Passphrase)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if doc.Config == nil {
		writeErr(w, http.StatusBadRequest, "this backup has no configuration in it")
		return
	}

	// The rollback snapshot first, with the same passphrase: it is the operator's
	// way back, and it must exist before anything is overwritten.
	rollback, err := s.writeRollbackSnapshot(body.Passphrase)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not write the rollback snapshot: "+err.Error())
		return
	}
	previous := *s.Cfg
	if err := s.saveConfig(doc.Config); err != nil {
		writeErr(w, http.StatusBadRequest, "could not write the restored configuration: "+err.Error())
		return
	}
	if err := s.Store.ImportSettings(s.Keyring, doc); err != nil {
		// Put the configuration back: a half-applied restore is worse than a
		// refused one.
		if rerr := s.saveConfig(&previous); rerr != nil {
			s.Log.Error("restore failed and the configuration could not be put back",
				"err", err, "rollback_err", rerr, "snapshot", rollback)
			writeErr(w, http.StatusInternalServerError, "restore failed and the configuration could not be "+
				"put back automatically; the rollback snapshot is at "+rollback+": "+err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, "restore failed and nothing was changed: "+err.Error())
		return
	}
	s.Log.Warn("settings restored from backup", "by", adminOf(r), "from_station", doc.Station,
		"backup_created", doc.Created.UTC().Format(time.RFC3339), "admins", len(doc.Admins),
		"ntrip_users", len(doc.Users), "rollback_snapshot", rollback)
	writeJSON(w, http.StatusOK, map[string]any{
		"restored": doc.Summary(), "rollback_snapshot": rollback,
		// Replacing the administrators dropped every session, this one included.
		"signed_out": true, "restarting": true,
	})
	go s.restartSoon()
}

// sealCurrentSettings builds the document and encrypts it.
func (s *Server) sealCurrentSettings(passphrase string) ([]byte, *backup.Document, error) {
	if s.Cfg == nil {
		return nil, nil, errors.New("no configuration is loaded")
	}
	if s.Keyring == nil {
		return nil, nil, errors.New("no master key is loaded, so stored passwords cannot be read")
	}
	cfg := *s.Cfg
	doc := &backup.Document{Created: time.Now().UTC(), Station: s.Cfg.Station.Name,
		AppVersion: version.String(), Config: &cfg}
	if err := s.Store.ExportSettings(s.Keyring, doc); err != nil {
		return nil, nil, err
	}
	blob, err := backup.Seal(doc, passphrase)
	if err != nil {
		return nil, nil, err
	}
	return blob, doc, nil
}

// writeRollbackSnapshot keeps a copy of the current settings beside the
// database, readable only by the daemon's user, and returns its path.
func (s *Server) writeRollbackSnapshot(passphrase string) (string, error) {
	blob, _, err := s.sealCurrentSettings(passphrase)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(s.Cfg.Telemetry.DBPath), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "pre-restore-"+time.Now().UTC().Format("20060102-150405")+".psbk")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

type uploadBody struct {
	Passphrase string `json:"passphrase"`
	Data       string `json:"data"` // base64 of the .psbk file
	blob       []byte
}

// readUpload accepts the file as base64 in JSON. A settings backup is a few
// kilobytes, so there is no reason for multipart handling or for a body larger
// than the one cap every write already has.
func readUpload(w http.ResponseWriter, r *http.Request) (*uploadBody, error) {
	var in uploadBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.Data))
	if err != nil {
		return nil, errors.New("the uploaded file could not be read")
	}
	if len(raw) == 0 {
		return nil, errors.New("no file was uploaded")
	}
	in.blob = raw
	return &in, nil
}

func (s *Server) decodeUpload(w http.ResponseWriter, r *http.Request) (*backup.Document, error) {
	body, err := readUpload(w, r)
	if err != nil {
		return nil, err
	}
	return backup.Open(body.blob, body.Passphrase)
}
