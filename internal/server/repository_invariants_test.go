package server

import (
	"net/http"
	"testing"
)

func TestGroupRejectsNestedMemberViaAPI(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	create := func(body string) *http.Response {
		return fixture.requestWithBearer(
			t, http.MethodPost, "/api/v1/repositories",
			[]byte(body), "application/json", testToken,
		)
	}

	leaf := create(`{"name":"leaf","format":"raw","type":"hosted"}`)
	assertStatus(t, leaf, http.StatusCreated)
	leaf.Body.Close()

	inner := create(`{"name":"inner","format":"raw","type":"group","members":["leaf"]}`)
	assertStatus(t, inner, http.StatusCreated)
	inner.Body.Close()

	// A group whose member is itself a group is rejected.
	outer := create(`{"name":"outer","format":"raw","type":"group","members":["inner"]}`)
	assertStatus(t, outer, http.StatusBadRequest)
	outer.Body.Close()
}

func TestUpdateProxyRejectsUpstreamEndpointChangeViaAPI(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	create := fixture.requestWithBearer(
		t, http.MethodPost, "/api/v1/repositories",
		[]byte(`{"name":"mirror","format":"raw","type":"proxy","upstream":"https://origin.example.test/raw"}`),
		"application/json", testToken,
	)
	assertStatus(t, create, http.StatusCreated)
	create.Body.Close()

	// Rotating only the credentials keeps the endpoint and is accepted.
	rotate := fixture.requestWithBearer(
		t, http.MethodPut, "/api/v1/repositories/mirror",
		[]byte(`{"format":"raw","type":"proxy","upstream":"https://user:secret@origin.example.test/raw"}`),
		"application/json", testToken,
	)
	assertStatus(t, rotate, http.StatusOK)
	rotate.Body.Close()

	// Repointing the endpoint is rejected as an immutable-field conflict.
	repoint := fixture.requestWithBearer(
		t, http.MethodPut, "/api/v1/repositories/mirror",
		[]byte(`{"format":"raw","type":"proxy","upstream":"https://elsewhere.example.test/raw"}`),
		"application/json", testToken,
	)
	assertStatus(t, repoint, http.StatusConflict)
	repoint.Body.Close()
}

func TestDeleteGroupReferencedRepositoryRejectedViaAPI(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)

	leaf := fixture.requestWithBearer(
		t, http.MethodPost, "/api/v1/repositories",
		[]byte(`{"name":"leaf","format":"raw","type":"hosted"}`), "application/json", testToken,
	)
	assertStatus(t, leaf, http.StatusCreated)
	leaf.Body.Close()

	group := fixture.requestWithBearer(
		t, http.MethodPost, "/api/v1/repositories",
		[]byte(`{"name":"grp","format":"raw","type":"group","members":["leaf"]}`), "application/json", testToken,
	)
	assertStatus(t, group, http.StatusCreated)
	group.Body.Close()

	rejected := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/repositories/leaf", nil, "", testToken,
	)
	assertStatus(t, rejected, http.StatusConflict)
	rejected.Body.Close()

	deleteGroup := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/repositories/grp", nil, "", testToken,
	)
	assertStatus(t, deleteGroup, http.StatusNoContent)
	deleteGroup.Body.Close()

	deleteLeaf := fixture.requestWithBearer(
		t, http.MethodDelete, "/api/v1/repositories/leaf", nil, "", testToken,
	)
	assertStatus(t, deleteLeaf, http.StatusNoContent)
	deleteLeaf.Body.Close()
}
