package web

import (
	"encoding/json"
	"io"
	"net/http"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
	"github.com/boxysh/boxy/internal/service"
)

// The dashboard calls the service directly rather than over HTTP, so it
// checks requests against the same protovalidate rules the API interceptor
// does.
func validate(msg proto.Message) error {
	if err := protovalidate.Validate(msg); err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return nil
}

func (h *Handler) signedIn(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := h.sessions.GetUserID(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	req := &boxyv1.CreateAccountRequest{Addr: r.FormValue("addr")}
	if name := r.FormValue("name"); name != "" {
		req.Name = &name
	}
	if err := validate(req); err != nil {
		writeError(w, "createAccount", err)
		return
	}
	resp, err := h.svc.CreateAccount(r.Context(), req)
	if err != nil {
		writeError(w, "createAccount", err)
		return
	}
	writeJSON(w, map[string]string{"id": resp.Account.Id})
}

func (h *Handler) createLabel(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	req := &boxyv1.CreateLabelRequest{Name: r.FormValue("name")}
	if err := validate(req); err != nil {
		writeError(w, "createLabel", err)
		return
	}
	if _, err := h.svc.CreateLabel(r.Context(), req); err != nil {
		writeError(w, "createLabel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteLabel(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	req := &boxyv1.DeleteLabelRequest{Name: r.PathValue("name")}
	if err := validate(req); err != nil {
		writeError(w, "deleteLabel", err)
		return
	}
	if _, err := h.svc.DeleteLabel(r.Context(), req); err != nil {
		writeError(w, "deleteLabel", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadMessage stores the request body, a raw message, in the account
// named by the account query parameter, as if it had just been delivered.
func (h *Handler) uploadMessage(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, service.MaxMessageSize))
	if err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	m, err := h.svc.Store(r.Context(), service.Incoming{AccountIDOrAddr: r.URL.Query().Get("account"), Raw: raw})
	if err != nil {
		writeError(w, "uploadMessage", err)
		return
	}
	writeJSON(w, map[string]string{"id": m.ID})
}

// updateLabels adds and removes a message's labels: the body is
// {"add_labels": [...], "remove_labels": [...]}, as in PATCH /v1/messages/{id}.
func (h *Handler) updateLabels(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	var body struct {
		AddLabels    []string `json:"add_labels"`
		RemoveLabels []string `json:"remove_labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req := &boxyv1.UpdateMessageRequest{Id: r.PathValue("id"), AddLabels: body.AddLabels, RemoveLabels: body.RemoveLabels}
	if err := validate(req); err != nil {
		writeError(w, "updateLabels", err)
		return
	}
	resp, err := h.svc.UpdateMessage(r.Context(), req)
	if err != nil {
		writeError(w, "updateLabels", err)
		return
	}
	writeJSON(w, map[string][]string{"labels": resp.Message.Labels})
}
