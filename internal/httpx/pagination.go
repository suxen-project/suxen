package httpx

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const (
	// DefaultCollectionLimit is the control-plane page size when limit is omitted.
	DefaultCollectionLimit = 100
	// MaximumCollectionLimit caps the control-plane page size.
	MaximumCollectionLimit = 200
	// MaximumCursorLength caps opaque collection cursors.
	MaximumCursorLength     = 2048
	maximumCursorJSONLength = 1024
)

// CollectionPage is a cursor-paginated JSON collection. Total is set only by the
// snapshot-offset paginator (WriteCollection), where the full result length is
// known, so clients can render numbered pages; the keyset paginator leaves it nil.
type CollectionPage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	Total      *int   `json:"total,omitempty"`
}

// CursorPayload is the snapshot-offset cursor encoded for collection pages.
type CursorPayload struct {
	Version  int    `json:"v"`
	Resource string `json:"r"`
	Offset   int    `json:"o"`
	Snapshot string `json:"s"`
}

type idPageCursorPayload struct {
	Version    int    `json:"v"`
	Resource   string `json:"r"`
	SnapshotID int64  `json:"s"`
	BeforeID   int64  `json:"b"`
}

// WriteCollection writes a snapshot-hashed offset page of values.
func WriteCollection[T any](
	w http.ResponseWriter,
	r *http.Request,
	resource string,
	values []T,
	key func(T) string,
) {
	limit, err := CollectionLimit(r)
	if err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}

	keys := make([]string, len(values))
	for index, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			WriteServerProblem(
				w,
				http.StatusInternalServerError,
				"pagination_error",
				"collection item cannot be encoded",
				err,
			)
			return
		}
		keys[index] = key(value) + "\x00" + string(encoded)
	}
	snapshot := collectionSnapshot(keys)
	offset, err := collectionOffset(r, resource, snapshot, len(values), limit)
	if err != nil {
		var stale *StaleCursorError
		if errors.As(err, &stale) {
			WriteProblem(w, http.StatusConflict, "stale_cursor", stale.Error())
			return
		}
		WriteProblem(w, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}

	end := len(values)
	if limit < len(values)-offset {
		end = offset + limit
	}
	items := make([]T, end-offset)
	copy(items, values[offset:end])
	total := len(values)
	page := CollectionPage[T]{Items: items, Total: &total}
	if end < len(values) {
		page.NextCursor = EncodeCursor(CursorPayload{
			Version:  1,
			Resource: resource,
			Offset:   end,
			Snapshot: snapshot,
		})
	}
	WriteJSON(w, http.StatusOK, page)
}

// CollectionLimit parses the limit query parameter.
func CollectionLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return DefaultCollectionLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > MaximumCollectionLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", MaximumCollectionLimit)
	}
	return limit, nil
}

func collectionOffset(
	r *http.Request,
	resource string,
	snapshot string,
	length int,
	limit int,
) (int, error) {
	query := r.URL.Query()
	if query.Has("page") && query.Has("cursor") {
		return 0, errors.New("page and cursor cannot be used together")
	}
	// A page parameter is random access for numbered pagination: it jumps to an
	// arbitrary offset without a cursor, so it does not carry the snapshot check.
	// A page past the end clamps to an empty tail rather than erroring.
	if raw := query.Get("page"); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return 0, errors.New("page must be a positive integer")
		}
		if page-1 > length/limit {
			return length, nil
		}
		return (page - 1) * limit, nil
	}
	if query.Has("page") {
		return 0, errors.New("page must be a positive integer")
	}
	encoded := query.Get("cursor")
	if encoded == "" {
		return 0, nil
	}
	var payload CursorPayload
	if err := decodeOpaqueCursor(encoded, &payload); err != nil {
		return 0, err
	}
	if payload.Version != 1 || payload.Resource == "" || payload.Offset <= 0 ||
		payload.Snapshot == "" {
		return 0, errors.New("cursor payload is invalid")
	}
	if payload.Resource != resource {
		return 0, &StaleCursorError{Reason: "cursor belongs to a different collection"}
	}
	if payload.Snapshot != snapshot || payload.Offset > length {
		return 0, &StaleCursorError{Reason: "collection changed after the cursor was issued"}
	}
	return payload.Offset, nil
}

// IDPageCursor parses a history-style id cursor.
func IDPageCursor(
	r *http.Request,
	resource string,
) (int64, int64, error) {
	encoded := r.URL.Query().Get("cursor")
	if encoded == "" {
		return 0, 0, nil
	}
	var payload idPageCursorPayload
	if err := decodeOpaqueCursor(encoded, &payload); err != nil {
		return 0, 0, err
	}
	if payload.Version != 1 || payload.Resource == "" || payload.SnapshotID <= 0 ||
		payload.BeforeID <= 0 || payload.BeforeID > payload.SnapshotID {
		return 0, 0, errors.New("cursor payload is invalid")
	}
	if payload.Resource != resource {
		return 0, 0, &StaleCursorError{Reason: "cursor belongs to a different collection"}
	}
	return payload.SnapshotID, payload.BeforeID, nil
}

// WriteIDCollectionPage writes a newest-first id-paginated collection.
func WriteIDCollectionPage[T any](
	w http.ResponseWriter,
	resource string,
	items []T,
	snapshotID int64,
	hasMore bool,
	id func(T) int64,
) {
	pageItems := make([]T, len(items))
	copy(pageItems, items)
	page := CollectionPage[T]{Items: pageItems}
	if hasMore && len(items) > 0 {
		page.NextCursor = EncodeCursor(idPageCursorPayload{
			Version:    1,
			Resource:   resource,
			SnapshotID: snapshotID,
			BeforeID:   id(items[len(items)-1]),
		})
	}
	WriteJSON(w, http.StatusOK, page)
}

// DecodeCursor parses a cursor written by EncodeCursor into destination,
// rejecting non-canonical encodings and unknown fields.
func DecodeCursor(encoded string, destination any) error {
	return decodeOpaqueCursor(encoded, destination)
}

func decodeOpaqueCursor(encoded string, destination any) error {
	if len(encoded) == 0 || len(encoded) > MaximumCursorLength {
		return errors.New("cursor length is invalid")
	}
	encoding := base64.RawURLEncoding.Strict()
	payloadBytes, err := encoding.DecodeString(encoded)
	if err != nil || len(payloadBytes) > maximumCursorJSONLength ||
		encoding.EncodeToString(payloadBytes) != encoded {
		return errors.New("cursor is not canonical base64url")
	}
	decoder := json.NewDecoder(bytes.NewReader(payloadBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("cursor payload is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("cursor payload contains trailing data")
	}
	canonical, err := json.Marshal(destination)
	if err != nil || !bytes.Equal(canonical, payloadBytes) {
		return errors.New("cursor payload is not canonical JSON")
	}
	return nil
}

func collectionSnapshot(keys []string) string {
	hash := sha256.New()
	for _, key := range keys {
		hash.Write([]byte(strconv.Itoa(len(key))))
		hash.Write([]byte{':'})
		hash.Write([]byte(key))
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

// EncodeCursor serializes a cursor payload as canonical base64url JSON.
func EncodeCursor(payload any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("encode collection cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// StaleCursorError is returned when a collection cursor no longer matches.
type StaleCursorError struct {
	Reason string
}

func (err *StaleCursorError) Error() string {
	return err.Reason
}

// WriteCursorError maps cursor parse/staleness errors onto HTTP problems.
func WriteCursorError(w http.ResponseWriter, err error) {
	var stale *StaleCursorError
	if errors.As(err, &stale) {
		WriteProblem(w, http.StatusConflict, "stale_cursor", stale.Error())
		return
	}
	WriteProblem(w, http.StatusBadRequest, "invalid_cursor", err.Error())
}
