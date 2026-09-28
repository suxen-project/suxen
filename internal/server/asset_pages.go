package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/suxen-project/suxen/internal/httpx"
	"github.com/suxen-project/suxen/internal/store"
)

type assetPageCursor struct {
	Version  int    `json:"v"`
	Resource string `json:"r"`
	AfterID  int64  `json:"a"`
	MaxID    int64  `json:"m"`
}

func assetPageResource(repositoryID, prefix string) string {
	digest := sha256.Sum256([]byte(repositoryID + "\x00" + prefix))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func parseAssetPageRequest(r *http.Request, repositoryID, prefix string, limit int) (store.AssetPageRequest, error) {
	request := store.AssetPageRequest{Prefix: prefix, Limit: limit}
	query := r.URL.Query()
	if _, hasPage := query["page"]; hasPage {
		return request, errors.New("page is not supported for asset listing; use cursor")
	}
	cursors, hasCursor := query["cursor"]
	if !hasCursor {
		return request, nil
	}
	if len(cursors) != 1 || len(cursors[0]) == 0 || len(cursors[0]) > httpx.MaximumCursorLength {
		return request, errors.New("cursor length is invalid")
	}
	encoded := cursors[0]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded || len(decoded) > 1024 {
		return request, errors.New("cursor encoding is invalid")
	}
	var cursor assetPageCursor
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return request, errors.New("cursor payload is invalid")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return request, errors.New("cursor payload is invalid")
	}
	if cursor.Version != 1 || cursor.AfterID < 1 || cursor.MaxID < cursor.AfterID {
		return request, errors.New("cursor payload is invalid")
	}
	if cursor.Resource != assetPageResource(repositoryID, prefix) {
		return request, &httpx.StaleCursorError{Reason: "cursor belongs to a different collection"}
	}
	request.AfterID = cursor.AfterID
	request.MaxID = cursor.MaxID
	return request, nil
}
