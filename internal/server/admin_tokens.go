package server

import (
	"context"
	"net/http"
	"strconv"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

type tokenCreateRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes,omitempty"`
}

type tokenCreatedResponse struct {
	domain.APIToken
	Token string `json:"token"`
}

// tokenStore is the API-token capability the token handlers need: list a user's
// tokens, mint a new one, and delete one by ID.
type tokenStore interface {
	Tokens(context.Context, string) ([]domain.APIToken, error)
	CreateToken(context.Context, string, string, string, []string) (domain.APIToken, error)
	DeleteToken(context.Context, string, int64) error
}

// tokenStore narrows the metadata store to the API-token capability. It reads
// s.metadata on each call so a reconfigured backend is honoured.
func (s *Server) tokenStore() tokenStore {
	return s.metadata
}

func (s *Server) handleUserTokens(
	w http.ResponseWriter,
	r *http.Request,
	username string,
) {
	if _, err := s.accountStore().User(r.Context(), username); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.tokenStore().Tokens(r.Context(), username)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCollection(w, r, "user-tokens:"+username, tokens, func(token domain.APIToken) string {
			return strconv.FormatInt(token.ID, 10)
		})
	case http.MethodPost:
		var request tokenCreateRequest
		if !httpx.DecodeJSON(w, r, &request) {
			return
		}
		name := request.Name
		if name == "" {
			name = "api"
		}
		token := randomSecret(32)
		created, err := s.tokenStore().CreateToken(r.Context(), username, name, token, request.Scopes)
		if err != nil {
			httpx.WriteResult(w, nil, err)
			return
		}
		httpx.WriteCreated(
			w,
			collectionItemLocation(r, strconv.FormatInt(created.ID, 10)),
			tokenCreatedResponse{APIToken: created, Token: token},
		)
	default:
		httpx.MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) handleUserTokenItem(
	w http.ResponseWriter,
	r *http.Request,
	username string,
	tokenIDValue string,
) {
	if r.Method != http.MethodDelete {
		httpx.MethodNotAllowed(w, http.MethodDelete)
		return
	}
	tokenID, err := strconv.ParseInt(tokenIDValue, 10, 64)
	if err != nil || tokenID <= 0 {
		httpx.WriteProblem(w, http.StatusBadRequest, "invalid_id", "token ID must be positive")
		return
	}
	if err := s.tokenStore().DeleteToken(r.Context(), username, tokenID); err != nil {
		httpx.WriteResult(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
