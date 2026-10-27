package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	boxyv1 "github.com/boxysh/boxy/gen/boxy/v1"
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
	req := &boxyv1.CreateAccountRequest{Addresses: []string{r.FormValue("address")}}
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
	writeJSON(w, map[string]string{"id": resp.Account.Id, "address": resp.Account.Addresses[0]})
}

// updateAccount changes an account's name and addresses: the body is
// {"name": ..., "addresses": [...]}, as in PATCH /v1/accounts/{id}.
func (h *Handler) updateAccount(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	var body struct {
		Name      *string  `json:"name"`
		Addresses []string `json:"addresses"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req := &boxyv1.UpdateAccountRequest{
		IdOrAddress: r.PathValue("id"),
		Name:        body.Name,
		Addresses:   body.Addresses,
	}
	if err := validate(req); err != nil {
		writeError(w, "updateAccount", err)
		return
	}
	resp, err := h.svc.UpdateAccount(r.Context(), req)
	if err != nil {
		writeError(w, "updateAccount", err)
		return
	}
	writeJSON(w, map[string]string{"address": resp.Account.Addresses[0]})
}

// share gives a user access to an account: the form has their sign-in
// email.
func (h *Handler) share(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	if err := h.svc.Share(r.Context(), r.PathValue("id"), r.FormValue("email")); err != nil {
		writeError(w, "share", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) unshare(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	if err := h.svc.Unshare(r.Context(), r.PathValue("id"), r.PathValue("user")); err != nil {
		writeError(w, "unshare", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// maxBulk is the most messages one bulk action changes: a page and then some.
const maxBulk = 100

// updateManyLabels adds and removes labels on several messages at once:
// the body is {"ids": [...], "add_labels": [...], "remove_labels": [...]}.
func (h *Handler) updateManyLabels(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	var body struct {
		IDs          []string `json:"ids"`
		AddLabels    []string `json:"add_labels"`
		RemoveLabels []string `json:"remove_labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > maxBulk {
		http.Error(w, fmt.Sprintf("select 1 to %d messages", maxBulk), http.StatusBadRequest)
		return
	}
	// Check every request first, so a bad one changes nothing.
	reqs := make([]*boxyv1.UpdateMessageRequest, len(body.IDs))
	for i, id := range body.IDs {
		reqs[i] = &boxyv1.UpdateMessageRequest{Id: id, AddLabels: body.AddLabels, RemoveLabels: body.RemoveLabels}
		if err := validate(reqs[i]); err != nil {
			writeError(w, "updateManyLabels", err)
			return
		}
	}
	updated := 0
	for _, req := range reqs {
		_, err := h.svc.UpdateMessage(r.Context(), req)
		if connect.CodeOf(err) == connect.CodeNotFound && strings.Contains(err.Error(), "message not found") {
			continue // deleted meanwhile
		}
		if err != nil {
			writeError(w, "updateManyLabels", err)
			return
		}
		updated++
	}
	writeJSON(w, map[string]int{"updated": updated})
}

// revokeKeys deletes all of a user's API keys.
func (h *Handler) revokeKeys(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	n, err := h.svc.RevokeKeys(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, "revokeKeys", err)
		return
	}
	writeJSON(w, map[string]int{"revoked": n})
}

// removeAccess takes a user off every account shared with them.
func (h *Handler) removeAccess(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	n, err := h.svc.RemoveAccess(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, "removeAccess", err)
		return
	}
	writeJSON(w, map[string]int{"removed": n})
}

// deleteMessages deletes several messages for good: the body is
// {"ids": [...]}.
func (h *Handler) deleteMessages(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > maxBulk {
		http.Error(w, fmt.Sprintf("select 1 to %d messages", maxBulk), http.StatusBadRequest)
		return
	}
	reqs := make([]*boxyv1.DeleteMessageRequest, len(body.IDs))
	for i, id := range body.IDs {
		reqs[i] = &boxyv1.DeleteMessageRequest{Id: id}
		if err := validate(reqs[i]); err != nil {
			writeError(w, "deleteMessages", err)
			return
		}
	}
	deleted := 0
	for _, req := range reqs {
		_, err := h.svc.DeleteMessage(r.Context(), req)
		if connect.CodeOf(err) == connect.CodeNotFound && strings.Contains(err.Error(), "message not found") {
			continue // deleted meanwhile
		}
		if err != nil {
			writeError(w, "deleteMessages", err)
			return
		}
		deleted++
	}
	writeJSON(w, map[string]int{"deleted": deleted})
}

// emptyLabel deletes for good all of an account's mail in the trash or
// spam: the body is {"label": "trash"}.
func (h *Handler) emptyLabel(w http.ResponseWriter, r *http.Request) {
	if !h.signedIn(w, r) {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	deleted, err := h.svc.EmptyLabel(r.Context(), r.PathValue("id"), body.Label)
	if err != nil {
		writeError(w, "emptyLabel", err)
		return
	}
	writeJSON(w, map[string]int{"deleted": deleted})
}
