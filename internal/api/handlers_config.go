package api

import (
	"net/http"
	"os"

	"github.com/Bravo-CDT/Scoring/internal/config"
)

type getConfigResponse struct {
	Text string `json:"text"`
	Hash string `json:"hash"`
}

// handleGetConfig serves GET /config: the config file's current raw bytes
// and their hash. The bytes are pre-env-expansion, same as the file an
// admin already hand-edits, so this exposes nothing beyond that file's
// existing trust level (admin-only).
func (s *server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, getConfigResponse{Text: string(raw), Hash: config.Hash(raw)})
}

type configStatusResponse struct {
	Stale bool `json:"stale"`
}

// handleConfigStatus serves GET /config/status: whether the file on disk
// differs from what the engine last successfully loaded, for the admin
// console's "New config detected" indicator.
func (s *server) handleConfigStatus(w http.ResponseWriter, r *http.Request) {
	raw, err := os.ReadFile(s.configPath)
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, configStatusResponse{Stale: config.Hash(raw) != s.engine.LoadedConfigHash()})
}

type saveConfigRequest struct {
	Text         string `json:"text"`
	ExpectedHash string `json:"expected_hash"`
}

type saveConfigResponse struct {
	Hash string `json:"hash"`
}

// handleSaveConfig serves POST /config: rejects a save based on a stale copy
// (409, nothing written), validates the new text with config.Parse before
// writing anything (400 on failure, nothing written), then writes it and
// answers with its new hash. It does not reload: taking effect is still the
// separate, existing POST /reload.
func (s *server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req saveConfigRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeBodyError(w, err)
		return
	}

	current, err := os.ReadFile(s.configPath)
	if err != nil {
		s.adminError(w, r, err)
		return
	}
	if config.Hash(current) != req.ExpectedHash {
		writeError(w, http.StatusConflict, "the config file has changed since you loaded it; reload and reapply your edit")
		return
	}

	raw := []byte(req.Text)
	if _, err := config.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := os.WriteFile(s.configPath, raw, 0o600); err != nil {
		s.adminError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, saveConfigResponse{Hash: config.Hash(raw)})
}
