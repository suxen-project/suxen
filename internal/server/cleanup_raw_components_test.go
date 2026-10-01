package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/assetattrs"
	"github.com/suxen-project/suxen/internal/domain"
)

const rawComponentRepository = "raw-components"

func rawComponentConfig(anchor bool) map[string]any {
	modelRule := map[string]any{"pattern": `^(?P<name>(models|tracks)/.+)/(?P<version>[0-9][^/]*)/[^/]+$`}
	if anchor {
		modelRule["anchor"] = `\.(glb|zip)$`
	}
	return map[string]any{"components": []any{
		modelRule,
		map[string]any{"pattern": `^(?P<name>client/alpha/[^/]+)/trackmaniac-(?P<version>[^/]+)-[^/]+\.zip$`},
	}}
}

func newRawComponentFixture(t *testing.T, formatConfig map[string]any) *serverFixture {
	t.Helper()
	f := newServerFixture(t)
	if err := f.Metadata.CreateRepository(context.Background(), domain.Repository{
		Name: rawComponentRepository, Format: "raw", Type: "hosted", FormatConfig: formatConfig,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// putRawComponentAssets publishes each path in order, spacing update times so
// updatedAt ordering is unambiguous.
func putRawComponentAssets(t *testing.T, f *serverFixture, paths ...string) {
	t.Helper()
	for _, assetPath := range paths {
		if _, err := f.Metadata.PutAsset(context.Background(), domain.Asset{
			Repository: rawComponentRepository, Path: assetPath, Kind: "raw",
			Digest: "sha256:" + strings.Repeat("a", 64), Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func assertRawAssets(t *testing.T, f *serverFixture, present []string, absent []string) {
	t.Helper()
	for _, assetPath := range present {
		if _, err := f.Metadata.Asset(context.Background(), rawComponentRepository, assetPath); err != nil {
			t.Errorf("%s lost: %v", assetPath, err)
		}
	}
	for _, assetPath := range absent {
		if _, err := f.Metadata.Asset(context.Background(), rawComponentRepository, assetPath); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s survived: %v", assetPath, err)
		}
	}
}

func coreComponentPolicy(keepLast int, order string) domain.CleanupPolicy {
	return domain.CleanupPolicy{
		Name: "keep-latest", KeepLast: keepLast, Order: order,
		Criteria: domain.CleanupCriteria{{Path: "raw.component", Op: "=", Value: "models/blocksets/core"}},
	}
}

func TestRawComponentCleanupDeletesWholeOlderVersionDirectory(t *testing.T) {
	f := newRawComponentFixture(t, rawComponentConfig(true))
	ctx := context.Background()
	older := []string{"models/blocksets/core/0.2.0/SHA256SUMS", "models/blocksets/core/0.2.0/core.glb"}
	newer := []string{"models/blocksets/core/0.2.1/SHA256SUMS", "models/blocksets/core/0.2.1/core.glb"}
	putRawComponentAssets(t, f, append(append([]string(nil), older...), newer...)...)
	policy := coreComponentPolicy(1, "")
	preview, err := f.Handler.cleanupRepository(ctx, policy, rawComponentRepository, true, time.Now())
	if err != nil || preview.Matched != 2 || !slices.Equal(preview.WouldDelete, older) {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	result, err := f.Handler.cleanupRepository(ctx, policy, rawComponentRepository, false, time.Now())
	if err != nil || result.Deleted != 2 || result.SkippedChanged != 0 {
		t.Fatalf("cleanup = %+v, %v", result, err)
	}
	assertRawAssets(t, f, newer, older)
}

func TestRawComponentCleanupKeepsUnitWhenSideFileDoesNotMatch(t *testing.T) {
	f := newRawComponentFixture(t, rawComponentConfig(true))
	ctx := context.Background()
	paths := []string{
		"models/blocksets/core/0.2.0/core.glb", "models/blocksets/core/0.2.0/SHA256SUMS",
		"models/blocksets/core/0.2.1/core.glb", "models/blocksets/core/0.2.1/SHA256SUMS",
	}
	putRawComponentAssets(t, f, paths...)
	policy := domain.CleanupPolicy{
		Name: "payload-only", KeepLast: 1,
		Criteria: domain.CleanupCriteria{{Path: "raw.path", Op: "matches", Value: `\.glb$`}},
	}
	result, err := f.Handler.cleanupRepository(ctx, policy, rawComponentRepository, false, time.Now())
	if err != nil || result.Deleted != 0 || result.Matched != 0 {
		t.Fatalf("cleanup = %+v, %v", result, err)
	}
	assertRawAssets(t, f, paths, nil)
}

func TestRawComponentAnchorIgnoresSideFileOnlyDirectory(t *testing.T) {
	f := newRawComponentFixture(t, rawComponentConfig(true))
	ctx := context.Background()
	older := []string{"models/blocksets/core/0.2.0/core.glb", "models/blocksets/core/0.2.0/SHA256SUMS"}
	newer := []string{"models/blocksets/core/0.2.1/core.glb", "models/blocksets/core/0.2.1/SHA256SUMS"}
	// The newest directory holds only checksums, so it must not take the
	// single keepLast slot from 0.2.1.
	orphan := "models/blocksets/core/0.3.0/SHA256SUMS"
	putRawComponentAssets(t, f, append(append(append([]string(nil), older...), newer...), orphan)...)

	repository, err := f.Metadata.Repository(ctx, rawComponentRepository)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := f.Metadata.ForRepository(repository).Assets(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	policy := coreComponentPolicy(0, "")
	units, ordinary := selectRetentionDirectoryUnits(policy, repository, repositoryRetentionGrouping(repository), repositoryRetentionUnitDirectory(repository), assets, time.Now())
	if len(units) != 2 || len(ordinary) != 1 || ordinary[0].Path != orphan {
		t.Fatalf("units=%d ordinary=%v, want 2 units and the orphan checksum file", len(units), ordinary)
	}

	result, err := f.Handler.cleanupRepository(ctx, coreComponentPolicy(1, ""), rawComponentRepository, false, time.Now())
	if err != nil || result.Deleted != 2 {
		t.Fatalf("cleanup = %+v, %v", result, err)
	}
	assertRawAssets(t, f, append(append([]string(nil), newer...), orphan), older)
}

func TestRawComponentCleanupKeepsUnmatchedPathBehavior(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
	}{
		{"without components", nil},
		{"with components", rawComponentConfig(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRawComponentFixture(t, tc.config)
			putRawComponentAssets(t, f, "docs/guide-1.txt", "docs/guide-2.txt", "notes/a.txt")
			policy := domain.CleanupPolicy{
				Name: "keep-one", KeepLast: 1,
				Criteria: domain.CleanupCriteria{{Path: "sys.path", Op: "exists"}},
			}
			result, err := f.Handler.cleanupRepository(context.Background(), policy, rawComponentRepository, false, time.Now())
			if err != nil || result.Deleted != 1 {
				t.Fatalf("cleanup = %+v, %v", result, err)
			}
			assertRawAssets(t, f, []string{"docs/guide-2.txt", "notes/a.txt"}, []string{"docs/guide-1.txt"})
		})
	}
}

func TestRawComponentCleanupVersionOrder(t *testing.T) {
	f := newRawComponentFixture(t, rawComponentConfig(true))
	ctx := context.Background()
	// The lower versions are published last, so updatedAt order would keep them.
	higher := []string{
		"models/blocksets/core/0.10.0/core.glb", "models/blocksets/core/0.10.0/SHA256SUMS",
		"client/alpha/linux/trackmaniac-1.10.0-x86_64.zip",
	}
	lower := []string{
		"models/blocksets/core/0.9.0/core.glb", "models/blocksets/core/0.9.0/SHA256SUMS",
		"client/alpha/linux/trackmaniac-1.9.0-x86_64.zip",
	}
	putRawComponentAssets(t, f, append(append([]string(nil), higher...), lower...)...)
	criteria := domain.CleanupCriteria{{Path: "raw.component", Op: "exists"}}
	sorted := func(paths []string) []string {
		paths = append([]string(nil), paths...)
		slices.Sort(paths)
		return paths
	}

	byUpdate := domain.CleanupPolicy{Name: "by-update", KeepLast: 1, Criteria: criteria}
	preview, err := f.Handler.cleanupRepository(ctx, byUpdate, rawComponentRepository, true, time.Now())
	if err != nil || !slices.Equal(sorted(preview.WouldDelete), sorted(higher)) {
		t.Fatalf("updatedAt preview = %+v, %v", preview, err)
	}

	byVersion := domain.CleanupPolicy{Name: "by-version", KeepLast: 1, Order: domain.CleanupOrderVersion, Criteria: criteria}
	result, err := f.Handler.cleanupRepository(ctx, byVersion, rawComponentRepository, false, time.Now())
	if err != nil || result.Deleted != len(lower) {
		t.Fatalf("version cleanup = %+v, %v", result, err)
	}
	assertRawAssets(t, f, higher, lower)
}

func TestOCICleanupVersionOrderUsesTags(t *testing.T) {
	now := time.Now().UTC()
	manifest := func(id int64, tag string, age time.Duration) domain.Asset {
		return domain.Asset{
			ID: id, Path: "v2/team/app/manifests/" + tag, Kind: "oci-manifest", Reference: tag,
			Digest: "sha256:" + strings.Repeat("b", 64), UpdatedAt: now.Add(-age),
		}
	}
	assets := []domain.Asset{manifest(1, "1.10.0", 2*time.Hour), manifest(2, "1.9.0", time.Hour)}
	policy := domain.CleanupPolicy{
		KeepLast: 1, Order: domain.CleanupOrderVersion,
		Criteria: domain.CleanupCriteria{{Path: "oci.image", Op: "=", Value: "team/app"}},
	}
	repository := domain.Repository{Name: "images", Format: "oci", Type: "hosted"}
	candidates, err := selectCleanupCandidates(policy, repository, nil, assets, now)
	if err != nil || len(candidates) != 1 || candidates[0].Reference != "1.9.0" {
		t.Fatalf("candidates = %+v, %v", candidates, err)
	}
}

func TestCompareVersionsUsesSemverThenNaturalOrder(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		want        int
	}{
		{"0.10.0", "0.9.0", 1},
		{"v1.2.3", "1.2.3", 0},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"build-10", "build-9", 1},
		{"2026.10.01", "2026.9.30", 1},
		{"alpha", "beta", -1},
	} {
		if got := compareVersions(tc.left, tc.right); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestRawComponentAttributesClassifyAndReclassifyOnRepositoryChange(t *testing.T) {
	f := newRawComponentFixture(t, nil)
	ctx := context.Background()
	if _, err := f.Metadata.SetClassification(ctx, domain.ClassificationConfig{
		Repository: rawComponentRepository, InheritGlobal: true,
		Rules: []domain.ClassificationRule{{
			When: []domain.Predicate{{Path: "raw.component", Op: "=", Value: "models/blocksets/core"}},
			Key:  "tier", Value: "model",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	paths := []string{"models/blocksets/core/0.2.0/core.glb", "models/blocksets/core/0.2.0/SHA256SUMS"}
	putRawComponentAssets(t, f, paths...)
	label := func(assetPath string) any {
		t.Helper()
		asset, err := f.Metadata.Asset(ctx, rawComponentRepository, assetPath)
		if err != nil {
			t.Fatal(err)
		}
		classification, _ := asset.Attributes["classification"].(map[string]any)
		return classification["tier"]
	}
	for _, assetPath := range paths {
		if got := label(assetPath); got != nil {
			t.Fatalf("%s labeled %v before components were declared", assetPath, got)
		}
	}

	repository, err := f.Metadata.Repository(ctx, rawComponentRepository)
	if err != nil {
		t.Fatal(err)
	}
	repository.FormatConfig = rawComponentConfig(true)
	if err := f.Metadata.UpdateRepository(ctx, repository); err != nil {
		t.Fatal(err)
	}
	for _, assetPath := range paths {
		if got := label(assetPath); got != "model" {
			t.Fatalf("%s label after component change = %v, want model", assetPath, got)
		}
	}
	putRawComponentAssets(t, f, "models/blocksets/core/0.2.1/SHA256SUMS")
	if got := label("models/blocksets/core/0.2.1/SHA256SUMS"); got != "model" {
		t.Fatalf("new side file label = %v, want model", got)
	}

	stored, err := f.Metadata.Asset(ctx, rawComponentRepository, "models/blocksets/core/0.2.0/SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := assetattrs.Project(stored, repository)["raw"].(map[string]any)
	if raw["component"] != "models/blocksets/core" || raw["version"] != "0.2.0" {
		t.Fatalf("projected raw attributes = %v", raw)
	}
}

func TestRawComponentConfigurationValidatedByAPI(t *testing.T) {
	f := newServerFixture(t)
	for _, body := range []string{
		`{"name":"bad","format":"raw","type":"hosted","formatConfig":{"components":[{"pattern":"^(?P<name>.+)/[^/]+$"}]}}`,
		`{"name":"bad","format":"raw","type":"hosted","formatConfig":{"components":[{"pattern":"("}]}}`,
		`{"name":"bad","format":"raw","type":"hosted","formatConfig":{"layout":"x"}}`,
	} {
		response := f.requestWithBearer(t, http.MethodPost, "/api/v1/repositories", []byte(body), "application/json", testToken)
		problem, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(problem), "invalid_format_config") {
			t.Fatalf("create %s = %d %s", body, response.StatusCode, problem)
		}
	}
	created := f.requestWithBearer(t, http.MethodPost, "/api/v1/repositories",
		[]byte(`{"name":"models","format":"raw","type":"hosted","formatConfig":{"components":[{"pattern":"^(?P<name>.+)/(?P<version>[^/]+)/[^/]+$","anchor":"\\.glb$"}]}}`),
		"application/json", testToken)
	assertStatus(t, created, http.StatusCreated)
	created.Body.Close()

	invalidOrder := f.requestWithBearer(t, http.MethodPost, "/api/v1/cleanup-policies",
		[]byte(`{"name":"sweep","repositories":["models"],"criteria":[{"path":"sys.path","op":"exists"}],"order":"name"}`),
		"application/json", testToken)
	problem, _ := io.ReadAll(invalidOrder.Body)
	invalidOrder.Body.Close()
	if invalidOrder.StatusCode != http.StatusBadRequest || !strings.Contains(string(problem), "invalid_cleanup_order") {
		t.Fatalf("invalid order = %d %s", invalidOrder.StatusCode, problem)
	}
	policy := f.requestWithBearer(t, http.MethodPost, "/api/v1/cleanup-policies",
		[]byte(`{"name":"sweep","repositories":["models"],"criteria":[{"path":"sys.path","op":"exists"}],"keepLast":1,"order":"version"}`),
		"application/json", testToken)
	assertStatus(t, policy, http.StatusCreated)
	defer policy.Body.Close()
	var stored domain.CleanupPolicy
	if err := json.NewDecoder(policy.Body).Decode(&stored); err != nil || stored.Order != domain.CleanupOrderVersion {
		t.Fatalf("stored policy = %+v, %v", stored, err)
	}
}

func trackmaniacConfig() map[string]any {
	return map[string]any{"components": []any{map[string]any{
		"pattern": `^(?P<name>client/alpha)/[^/]+/trackmaniac-(?P<version>[^/-]+)-[^/]+$`,
		"anchor":  `\.zip$`,
	}}}
}

func trackmaniacBuild(build string) []string {
	var paths []string
	for _, platform := range []string{"linux", "windows"} {
		zip := "client/alpha/" + platform + "/trackmaniac-alpha." + build + "-x86_64.zip"
		paths = append(paths, zip, zip+".sha256")
	}
	return paths
}

func TestRawComponentFileNamedVersionsFormOneUnitAcrossPlatforms(t *testing.T) {
	f := newRawComponentFixture(t, trackmaniacConfig())
	ctx := context.Background()
	builds := []string{"11150", "11151", "11152", "11153"}
	for _, build := range builds {
		putRawComponentAssets(t, f, trackmaniacBuild(build)...)
	}
	// A checksum without its zip never forms a build, so it cannot take a slot.
	orphan := "client/alpha/linux/trackmaniac-alpha.11160-x86_64.zip.sha256"
	putRawComponentAssets(t, f, orphan)
	policy := domain.CleanupPolicy{
		Name: "keep-three-builds", KeepLast: 3, Order: domain.CleanupOrderVersion,
		Criteria: domain.CleanupCriteria{{Path: "raw.component", Op: "=", Value: "client/alpha"}},
	}
	result, err := f.Handler.cleanupRepository(ctx, policy, rawComponentRepository, false, time.Now())
	if err != nil || result.Deleted != 4 {
		t.Fatalf("cleanup = %+v, %v", result, err)
	}
	var kept []string
	for _, build := range builds[1:] {
		kept = append(kept, trackmaniacBuild(build)...)
	}
	assertRawAssets(t, f, append(kept, orphan), trackmaniacBuild("11150"))
}

func TestRawComponentFileNamedUnitKeptWhenSideFileDoesNotMatch(t *testing.T) {
	f := newRawComponentFixture(t, trackmaniacConfig())
	ctx := context.Background()
	paths := append(trackmaniacBuild("11150"), trackmaniacBuild("11151")...)
	putRawComponentAssets(t, f, paths...)
	policy := domain.CleanupPolicy{
		Name: "zips-only", KeepLast: 1,
		Criteria: domain.CleanupCriteria{{Path: "raw.path", Op: "matches", Value: `\.zip$`}},
	}
	result, err := f.Handler.cleanupRepository(ctx, policy, rawComponentRepository, false, time.Now())
	if err != nil || result.Deleted != 0 {
		t.Fatalf("cleanup = %+v, %v", result, err)
	}
	assertRawAssets(t, f, paths, nil)
}

func TestRawComponentFileNamedUnitPreservedWhenSiblingAppears(t *testing.T) {
	f := newRawComponentFixture(t, trackmaniacConfig())
	ctx := context.Background()
	putRawComponentAssets(t, f, trackmaniacBuild("11150")...)
	repository, err := f.Metadata.Repository(ctx, rawComponentRepository)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.Metadata.ForRepository(repository).Assets(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	retention, ok := rawRetention(repository)
	if !ok {
		t.Fatal("repository has no component rules")
	}
	prefix, member, ok := retention.UnitMember(snapshot[0].Path)
	if !ok {
		t.Fatal("snapshot path is not a component member")
	}
	// A macOS build of the same version is published after selection.
	putRawComponentAssets(t, f, "client/alpha/macos/trackmaniac-alpha.11150-arm64.zip")
	deleted, err := f.Metadata.DeleteAssetSetIfUnchanged(ctx, prefix, member, snapshot)
	if err != nil || deleted {
		t.Fatalf("delete with new sibling = %t, %v", deleted, err)
	}
	assertRawAssets(t, f, trackmaniacBuild("11150"), nil)
}

func TestRawComponentsListing(t *testing.T) {
	f := newRawComponentFixture(t, rawComponentConfig(true))
	putRawComponentAssets(t, f,
		"models/blocksets/core/0.9.0/core.glb", "models/blocksets/core/0.9.0/SHA256SUMS",
		"models/blocksets/core/0.10.0/core.glb", "models/blocksets/core/0.10.0/SHA256SUMS",
		"models/blocksets/core/0.11.0/SHA256SUMS",
		"client/alpha/linux/trackmaniac-1.4.0-x86_64.zip",
		"docs/readme.txt",
	)
	type page struct {
		Items      []repositoryComponentVersion `json:"items"`
		NextCursor string                       `json:"nextCursor"`
	}
	var rows []repositoryComponentVersion
	query := "?limit=1"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("listing did not terminate")
		}
		response := f.request(t, http.MethodGet, "/api/v1/repositories/"+rawComponentRepository+"/components"+query, nil, true)
		assertStatus(t, response, http.StatusOK)
		var decoded page
		err := json.NewDecoder(response.Body).Decode(&decoded)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, decoded.Items...)
		if decoded.NextCursor == "" {
			break
		}
		query = "?limit=1&cursor=" + decoded.NextCursor
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Component+"@"+row.Version)
	}
	want := []string{"client/alpha/linux@1.4.0", "models/blocksets/core@0.10.0", "models/blocksets/core@0.9.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("listed versions = %v, want %v", got, want)
	}
	latest := rows[1]
	if len(latest.Assets) != 2 || latest.Assets[0].Path != "models/blocksets/core/0.10.0/SHA256SUMS" ||
		latest.Reference != "models/blocksets/core/0.10.0/core.glb" || latest.Size != 2 || latest.UpdatedAt.IsZero() {
		t.Fatalf("version row = %+v", latest)
	}

	unauthenticated := f.request(t, http.MethodGet, "/api/v1/repositories/"+rawComponentRepository+"/components", nil, false)
	unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated listing = %d", unauthenticated.StatusCode)
	}
}
