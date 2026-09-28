package cargo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

type failingWireTools struct {
	format.WireTools
	err      error
	reported []error
}

func (tools *failingWireTools) VisitAssetPaths(context.Context, string, func(string) (bool, error)) error {
	return tools.err
}

func (tools *failingWireTools) ReportWireError(_ context.Context, _ string, err error) {
	tools.reported = append(tools.reported, err)
}

func TestHostedIndexHidesInternalFailure(t *testing.T) {
	const secret = "sqlite-password-must-not-leak"
	tools := &failingWireTools{err: errors.New(secret)}
	request := httptest.NewRequest(http.MethodGet, "/repository/hosted/wi/dg/widget", nil)
	response := httptest.NewRecorder()
	Format{}.ServeWire(response, request, format.Repository{Name: "hosted", Type: "hosted"}, "wi/dg/widget", tools)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if len(tools.reported) != 1 || !errors.Is(tools.reported[0], tools.err) {
		t.Fatalf("reported = %v", tools.reported)
	}
}

func TestPublicationErrorCategoriesStaySpecific(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		report bool
	}{
		{format.ErrConflict, http.StatusConflict, false},
		{format.ErrPolicyRejected, http.StatusForbidden, false},
		{format.ErrUploadLimit, http.StatusRequestEntityTooLarge, false},
		{errors.New("sql-secret-must-not-leak"), http.StatusInternalServerError, true},
	} {
		tools := &failingWireTools{}
		response := httptest.NewRecorder()
		writePublicationError(response, httptest.NewRequest(http.MethodPut, "/", nil), tools, test.err)
		wantReports := 0
		if test.report {
			wantReports = 1
		}
		if response.Code != test.status || len(tools.reported) != wantReports || strings.Contains(response.Body.String(), "sql-secret-must-not-leak") {
			t.Fatalf("%v: status %d, reports %v", test.err, response.Code, tools.reported)
		}
	}
}
