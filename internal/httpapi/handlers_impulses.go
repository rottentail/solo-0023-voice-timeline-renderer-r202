package httpapi

import (
	"net/http"

	"voice-timeline-renderer/internal/service"
)

func (server *Server) handleImpulseWrite(w http.ResponseWriter, request *http.Request) {
	var payload service.ImpulseWriteRequest
	if err := decodeJSON(w, request, &payload, server.options.MaxSourceRequestBytes); err != nil {
		writeError(w, err, nil)
		return
	}
	asset, created, err := server.service.CreateImpulse(request.Context(), request.PathValue("id"), payload)
	if err != nil {
		writeError(w, err, nil)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, asset)
}

func (server *Server) handleImpulseRead(w http.ResponseWriter, request *http.Request) {
	asset, err := server.service.GetImpulse(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, asset)
}
