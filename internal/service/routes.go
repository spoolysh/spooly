package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"connectrpc.com/validate"
	"connectrpc.com/vanguard"

	"github.com/spoolysh/spooly/gen/spooly/v1/spoolyv1connect"
	"github.com/spoolysh/spooly/internal/auth"
)

// Mount registers the API on mux: Connect, gRPC, and gRPC-Web under
// /spooly.v1.*Service/, and REST under /v1/. Every route authenticates with
// API keys.
func (s *Service) Mount(mux *http.ServeMux) error {
	return s.mount(mux, s.requireAPIKey)
}

// requireAPIKey authenticates requests with a Bearer API key and puts the
// key's user in the request context.
func (s *Service) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			apiError(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		key, err := s.db.LookupAPIKey(r.Context(), token)
		if err != nil {
			apiError(w, "invalid api key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUserID(r.Context(), key.UserID)))
	})
}

// mount is Mount with the authentication supplied, so tests can skip it.
func (s *Service) mount(mux *http.ServeMux, authenticate func(http.Handler) http.Handler) error {
	server := connect.NewServer(validate.NewServerInterceptor())
	spoolyv1connect.RegisterMailboxServiceHandler(server, s)
	spoolyv1connect.RegisterLabelServiceHandler(server, s)
	spoolyv1connect.RegisterMessageServiceHandler(server, s)

	connecthttp.Mount(wrappingMux{mux, authenticate}, server)

	// Vanguard installs a catch-all "/", so it gets its own mux under /v1/.
	rest := http.NewServeMux()
	err := vanguard.Mount(rest, server,
		// The limit covers a whole streamed request body; CreateMessage
		// enforces MaxMessageSize itself.
		vanguard.WithMaxReadBytes(0),
		vanguard.WithDiscardUnknownQueryParams(true),
		vanguard.WithCodec(func(res connectproto.TypeResolver) vanguard.RESTCodec {
			codec := vanguard.NewJSONCodec(res)
			codec.MarshalOptions.UseProtoNames = true
			codec.MarshalOptions.EmitUnpopulated = true
			codec.UnmarshalOptions.DiscardUnknown = true
			return codec
		}),
	)
	if err != nil {
		return fmt.Errorf("mounting REST routes: %w", err)
	}
	mux.Handle("/v1/", authenticate(rest))
	return nil
}

// wrappingMux registers each handler on mux wrapped in wrap, so routes that
// connecthttp.Mount picks get the same middleware as ours.
type wrappingMux struct {
	mux  *http.ServeMux
	wrap func(http.Handler) http.Handler
}

func (m wrappingMux) Handle(pattern string, h http.Handler) {
	m.mux.Handle(pattern, m.wrap(h))
}

// apiError replies like http.Error, but with the JSON body Vanguard writes for
// failed RPCs, such as {"code": 16, "message": "invalid api key",
// "details": []}, so every /v1/ endpoint fails the same way.
func apiError(w http.ResponseWriter, message string, status int) {
	code := connect.CodeUnknown
	switch status {
	case http.StatusUnauthorized:
		code = connect.CodeUnauthenticated
	case http.StatusInternalServerError:
		code = connect.CodeInternal
	}

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Code    int32  `json:"code"` // a number, not connect.Code's text form
		Message string `json:"message"`
		Details []any  `json:"details"`
	}{int32(code), message, []any{}})
}
