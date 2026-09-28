package httpx

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func decodeIntPage(t *testing.T, body []byte) CollectionPage[int] {
	t.Helper()
	var page CollectionPage[int]
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	return page
}

func writeInts(t *testing.T, query string, count int) CollectionPage[int] {
	t.Helper()
	values := make([]int, count)
	for i := range values {
		values[i] = i
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/collection?"+query, nil)
	WriteCollection(recorder, request, "ints", values, func(value int) string {
		return string(rune(value))
	})
	response := recorder.Result()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.StatusCode, recorder.Body.String())
	}
	return decodeIntPage(t, recorder.Body.Bytes())
}

func TestWriteCollectionReportsTotal(t *testing.T) {
	page := writeInts(t, "limit=10", 25)
	if page.Total == nil || *page.Total != 25 {
		t.Fatalf("total = %v, want 25", page.Total)
	}
	if len(page.Items) != 10 {
		t.Fatalf("first page items = %d, want 10", len(page.Items))
	}
	if page.Items[0] != 0 {
		t.Fatalf("first page starts at %d, want 0", page.Items[0])
	}
}

func TestWriteCollectionPageParameterJumpsToOffset(t *testing.T) {
	page := writeInts(t, "limit=10&page=3", 25)
	if len(page.Items) != 5 {
		t.Fatalf("page 3 items = %d, want 5", len(page.Items))
	}
	if page.Items[0] != 20 {
		t.Fatalf("page 3 starts at %d, want 20", page.Items[0])
	}
	if page.Total == nil || *page.Total != 25 {
		t.Fatalf("total = %v, want 25", page.Total)
	}
}

func TestWriteCollectionPagePastEndIsEmpty(t *testing.T) {
	page := writeInts(t, "limit=10&page=99", 25)
	if len(page.Items) != 0 {
		t.Fatalf("page past end items = %d, want 0", len(page.Items))
	}
	if page.Total == nil || *page.Total != 25 {
		t.Fatalf("total = %v, want 25", page.Total)
	}
}

func TestWriteCollectionRejectsInvalidPage(t *testing.T) {
	for _, query := range []string{"page=0", "page=-1", "page=NaN", "page=", "page=1&cursor=x"} {
		t.Run(query, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/collection?"+query, nil)
			WriteCollection(recorder, request, "ints", []int{1, 2, 3}, func(value int) string {
				return string(rune(value))
			})
			if recorder.Result().StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Result().StatusCode)
			}
		})
	}
}

func TestWriteCollectionLargePagesReturnEmptyTail(t *testing.T) {
	for _, count := range []int{0, 2} {
		for _, pageNumber := range []int{math.MaxInt, math.MaxInt/100 + 2} {
			page := writeInts(t, fmt.Sprintf("page=%d&limit=100", pageNumber), count)
			if len(page.Items) != 0 || page.Total == nil || *page.Total != count {
				t.Fatalf("page %d with %d items = %+v", pageNumber, count, page)
			}
		}
	}
}
