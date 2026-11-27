package setupmode

import (
	"encoding/json"
	"errors"
	"net/http"
)

// These handlers are safe on a nil *Service: boards without the setup
// button then report {"available": false}, and the wizard skips its Wi-Fi
// step.

// HandleStatus serves GET /api/wifi/status.
func (s *Service) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.Status())
}

// HandleScan serves GET /api/wifi/scan; ?force=1 scans anew.
func (s *Service) HandleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"available": false, "networks": []Network{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"available": true, "networks": s.Networks(r.URL.Query().Get("force") == "1")})
}

// HandleConnect serves POST /api/wifi/connect {"ssid","password","security"}.
// It answers 202 before switching networks (see Service.Connect).
func (s *Service) HandleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Wi-Fi setup is not available on this device"})
		return
	}
	var body struct {
		SSID     string `json:"ssid"`
		Password string `json:"password"`
		Security string `json:"security"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := s.Connect(body.SSID, body.Password, body.Security); err != nil {
		writeJSON(w, requestStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

// HandleAP serves POST /api/wifi/ap {"action":"start"|"stop"}.
func (s *Service) HandleAP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Wi-Fi setup is not available on this device"})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	var err error
	switch body.Action {
	case "start":
		err = s.StartAP()
	case "stop":
		err = s.StopAP()
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `action must be "start" or "stop"`})
		return
	}
	if err != nil {
		writeJSON(w, requestStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func requestStatus(err error) int {
	if errors.Is(err, ErrBusy) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
