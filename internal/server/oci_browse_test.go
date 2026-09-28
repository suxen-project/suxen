package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/suxen-project/suxen/internal/domain"
	"github.com/suxen-project/suxen/internal/httpx"
)

func TestRepositoryComponentsGroupsOCIManifestsAndExcludesBlobs(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, tag := range []string{"stable", "latest"} {
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository:  "oci",
			Path:        "v2/acme/widget/manifests/" + tag,
			Digest:      "sha256:manifest-" + tag,
			ContentType: ociManifestMediaTypeForTest,
			Kind:        "oci-manifest",
			Reference:   tag,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A layer blob under the same image must not appear as a browsable version.
	if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "oci",
		Path:       "v2/acme/widget/blobs/sha256:layer",
		Digest:     "sha256:layer",
		Kind:       "oci-blob",
	}); err != nil {
		t.Fatal(err)
	}

	response := fixture.request(t, http.MethodGet, "/api/v1/repositories/oci/components", nil, true)
	assertStatus(t, response, http.StatusOK)
	var page httpx.CollectionPage[repositoryComponentVersion]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if len(page.Items) != 2 {
		t.Fatalf("components = %+v, want two manifest versions", page.Items)
	}
	for _, version := range page.Items {
		if version.Component != "acme/widget" {
			t.Fatalf("component = %q, want acme/widget", version.Component)
		}
		if version.Kind != "oci-manifest" {
			t.Fatalf("version %+v is not a manifest", version)
		}
	}
}

func TestRepositoryComponentsBoundsVersionsWithinOneComponent(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	const versionCount = 43
	for index := range versionCount {
		tag := fmt.Sprintf("tag-%03d", index)
		if _, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository: "oci", Path: "v2/acme/widget/manifests/" + tag,
			Digest: "sha256:" + tag, Kind: "oci-manifest", Reference: tag,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[int64]bool)
	cursor := ""
	for {
		url := "/api/v1/repositories/oci/components?limit=7"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		response := fixture.request(t, http.MethodGet, url, nil, true)
		assertStatus(t, response, http.StatusOK)
		var page httpx.CollectionPage[repositoryComponentVersion]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if len(page.Items) == 0 || len(page.Items) > 7 || page.Total != nil {
			t.Fatalf("unexpected bounded page: %+v", page)
		}
		for _, item := range page.Items {
			if item.Component != "acme/widget" || seen[item.AssetID] {
				t.Fatalf("unexpected or repeated component version: %+v", item)
			}
			seen[item.AssetID] = true
		}
		cursor = page.NextCursor
		if len(seen) == 7 {
			badPage := fixture.request(t, http.MethodGet, "/api/v1/repositories/oci/components?page=1", nil, true)
			assertStatus(t, badPage, http.StatusBadRequest)
			badPage.Body.Close()
			wrongRoute := fixture.request(t, http.MethodGet, "/api/v1/repositories/oci/browse?cursor="+cursor, nil, true)
			assertStatus(t, wrongRoute, http.StatusConflict)
			wrongRoute.Body.Close()
		}
		if cursor == "" {
			break
		}
	}
	if len(seen) != versionCount {
		t.Fatalf("got %d distinct versions, want %d", len(seen), versionCount)
	}
}

func TestRepositoryComponentsGroupKeepsFirstMemberPath(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "oci-second", Format: "oci", Type: "hosted"},
		{Name: "oci-group", Format: "oci", Type: "group", Members: []string{"oci", "oci-second"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	for _, asset := range []domain.Asset{
		{Repository: "oci", Path: "v2/acme/widget/manifests/shared", Digest: "sha256:first", Kind: "oci-manifest", Reference: "shared"},
		{Repository: "oci-second", Path: "v2/acme/widget/manifests/shared", Digest: "sha256:second", Kind: "oci-manifest", Reference: "shared"},
		{Repository: "oci-second", Path: "v2/acme/widget/manifests/other", Digest: "sha256:other", Kind: "oci-manifest", Reference: "other"},
	} {
		if _, err := fixture.Metadata.PutAsset(ctx, asset); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	cursor := ""
	finished := false
	for pageNumber := 0; pageNumber < 5; pageNumber++ {
		url := "/api/v1/repositories/oci-group/components?limit=1"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		response := fixture.request(t, http.MethodGet, url, nil, true)
		assertStatus(t, response, http.StatusOK)
		var page httpx.CollectionPage[repositoryComponentVersion]
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if len(page.Items) > 1 || page.Total != nil {
			t.Fatalf("unbounded group page = %+v", page)
		}
		for _, item := range page.Items {
			seen[item.Reference] = item.Digest
		}
		cursor = page.NextCursor
		if cursor == "" {
			finished = true
			break
		}
	}
	if !finished || len(seen) != 2 || seen["shared"] != "sha256:first" || seen["other"] != "sha256:other" {
		t.Fatalf("group versions = %+v", seen)
	}
}

func TestOCIGroupComponentAssetDrillIn(t *testing.T) {
	fixture := newServerFixture(t)
	ctx := context.Background()
	for _, repository := range []domain.Repository{
		{Name: "oci-second", Format: "oci", Type: "hosted"},
		{Name: "oci-other", Format: "oci", Type: "hosted"},
		{Name: "oci-group", Format: "oci", Type: "group", Members: []string{"oci", "oci-second"}},
	} {
		if err := fixture.Metadata.CreateRepository(ctx, repository); err != nil {
			t.Fatal(err)
		}
	}
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:config","size":7},"layers":[]}`)
	digest := testDigest(manifest)
	if _, err := fixture.Handler.blobs.Put(ctx, digest, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	put := func(repository, path, assetDigest string) domain.Asset {
		t.Helper()
		asset, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
			Repository: repository, Path: path, Digest: assetDigest,
			Size: int64(len(manifest)), ContentType: ociManifestMediaTypeForTest,
			Kind: "oci-manifest", Reference: "stable",
		})
		if err != nil {
			t.Fatal(err)
		}
		return asset
	}
	const path = "v2/acme/widget/manifests/stable"
	first := put("oci", path, digest)
	shadowed := put("oci-second", path, "sha256:shadowed")
	second := put("oci-second", "v2/acme/widget/manifests/other", digest)
	unrelated := put("oci-other", "v2/other/widget/manifests/stable", "sha256:unrelated")

	response := fixture.request(t, http.MethodGet, "/api/v1/repositories/oci-group/components", nil, true)
	assertStatus(t, response, http.StatusOK)
	var page httpx.CollectionPage[repositoryComponentVersion]
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(page.Items) != 2 || page.Items[0].AssetID != first.ID || page.Items[1].AssetID != second.ID {
		t.Fatalf("group component versions = %+v, want visible member assets %d and %d", page.Items, first.ID, second.ID)
	}
	for _, want := range []domain.Asset{first, second} {
		assetPath := fmt.Sprintf("/api/v1/repositories/oci-group/assets/%d", want.ID)
		response = fixture.request(t, http.MethodGet, assetPath, nil, true)
		assertStatus(t, response, http.StatusOK)
		var asset domain.Asset
		if err := json.NewDecoder(response.Body).Decode(&asset); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if asset.ID != want.ID || asset.Repository != "oci-group" || asset.Path != want.Path {
			t.Fatalf("group asset detail = %+v", asset)
		}
		response = fixture.request(t, http.MethodGet, assetPath+"/manifest", nil, true)
		assertStatus(t, response, http.StatusOK)
		var contents manifestContents
		if err := json.NewDecoder(response.Body).Decode(&contents); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if contents.Digest != digest || contents.Config == nil || contents.Config.Digest != "sha256:config" {
			t.Fatalf("group manifest = %+v", contents)
		}
	}
	assetPath := fmt.Sprintf("/api/v1/repositories/oci-group/assets/%d", first.ID)
	for _, hidden := range []domain.Asset{shadowed, unrelated} {
		for _, suffix := range []string{"", "/manifest"} {
			response = fixture.request(t, http.MethodGet,
				fmt.Sprintf("/api/v1/repositories/oci-group/assets/%d%s", hidden.ID, suffix), nil, true)
			assertStatus(t, response, http.StatusNotFound)
			response.Body.Close()
		}
	}
	response = fixture.request(t, http.MethodDelete, assetPath, nil, true)
	assertStatus(t, response, http.StatusMethodNotAllowed)
	response.Body.Close()
	response = fixture.request(t, http.MethodPut, assetPath+"/attributes/notes", []byte(`{"owner":"other"}`), true)
	assertStatus(t, response, http.StatusMethodNotAllowed)
	response.Body.Close()
	if _, err := fixture.Metadata.AssetByID(ctx, "oci", first.ID); err != nil {
		t.Fatalf("group delete changed member asset: %v", err)
	}
}

func TestAssetManifestReturnsConfigAndLayers(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	manifest := []byte(`{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:config","size":7},` +
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:layer","size":100}]}`)
	manifestDigest := testDigest(manifest)
	if _, err := fixture.Handler.blobs.Put(ctx, manifestDigest, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	asset, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository:  "oci",
		Path:        "v2/acme/widget/manifests/stable",
		Digest:      manifestDigest,
		Size:        int64(len(manifest)),
		ContentType: "application/vnd.oci.image.manifest.v1+json",
		Kind:        "oci-manifest",
		Reference:   "stable",
	})
	if err != nil {
		t.Fatal(err)
	}

	response := fixture.request(
		t, http.MethodGet,
		fmt.Sprintf("/api/v1/repositories/oci/assets/%d/manifest", asset.ID),
		nil, true,
	)
	assertStatus(t, response, http.StatusOK)
	var contents manifestContents
	if err := json.NewDecoder(response.Body).Decode(&contents); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if contents.Config == nil || contents.Config.Digest != "sha256:config" || contents.Config.Size != 7 {
		t.Fatalf("config = %+v, want digest sha256:config size 7", contents.Config)
	}
	if len(contents.Layers) != 1 || contents.Layers[0].Digest != "sha256:layer" || contents.Layers[0].Size != 100 {
		t.Fatalf("layers = %+v, want one layer sha256:layer size 100", contents.Layers)
	}
	if contents.Digest != manifestDigest {
		t.Fatalf("digest = %q, want %q", contents.Digest, manifestDigest)
	}
}

func TestAssetManifestRejectsNonManifestAsset(t *testing.T) {
	t.Parallel()
	fixture := newServerFixture(t)
	ctx := context.Background()
	asset, err := fixture.Metadata.PutAsset(ctx, domain.Asset{
		Repository: "oci",
		Path:       "v2/acme/widget/blobs/sha256:layer",
		Digest:     "sha256:layer",
		Kind:       "oci-blob",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := fixture.request(
		t, http.MethodGet,
		fmt.Sprintf("/api/v1/repositories/oci/assets/%d/manifest", asset.ID),
		nil, true,
	)
	assertStatus(t, response, http.StatusBadRequest)
	response.Body.Close()
}
