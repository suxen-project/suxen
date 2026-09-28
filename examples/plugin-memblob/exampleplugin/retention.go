package exampleplugin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

// exampleCoordinate recognizes a versioned artifact and excludes its metadata
// companion from keepLast counting.
func exampleCoordinate(path string) (name, version string, ok bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "pkg" || parts[3] != "artifact.txt" ||
		!exampleSegment(parts[1]) || !exampleSegment(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func exampleSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func (textFormat) RetentionGroupKey(_ format.Repository, asset format.Asset) (string, bool) {
	name, _, ok := exampleCoordinate(asset.Path)
	if !ok {
		return "", false
	}
	return "pkg/" + name, true
}

func (textFormat) CompanionPaths(_ format.Repository, assetPath string) []string {
	name, version, ok := exampleCoordinate(assetPath)
	if !ok {
		return nil
	}
	return []string{"pkg/" + name + "/" + version + "/meta.json"}
}

// The example's publish route demonstrates atomic artifact+metadata
// publication through the public WireTools contract.
func (textFormat) WireAction(repository format.Repository, method, requestPath string, _ url.Values) (string, bool) {
	if repository.Type != "hosted" || method != http.MethodPost {
		return "", false
	}
	_, _, ok := examplePublishCoordinate(requestPath)
	if !ok {
		return "", false
	}
	return "write", true
}

func examplePublishCoordinate(requestPath string) (name, version string, ok bool) {
	parts := strings.Split(requestPath, "/")
	if len(parts) != 4 || parts[0] != "_example" || parts[1] != "publish" ||
		!exampleSegment(parts[2]) || !exampleSegment(parts[3]) {
		return "", "", false
	}
	return parts[2], parts[3], true
}

func (textFormat) ServeWire(w http.ResponseWriter, r *http.Request, _ format.Repository, requestPath string, tools format.WireTools) {
	name, version, ok := examplePublishCoordinate(requestPath)
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	artifact := "pkg/" + name + "/" + version + "/artifact.txt"
	meta := "pkg/" + name + "/" + version + "/meta.json"
	content := http.MaxBytesReader(w, r.Body, tools.MaxUploadBytes())
	defer content.Close()
	_, err := tools.StoreAssets(r.Context(), []format.AssetInput{
		{Path: artifact, ContentType: "text/plain", Content: content},
		{Path: meta, ContentType: "application/json", Content: strings.NewReader(`{"version":"` + version + `"}`), Metadata: true},
	})
	if err != nil {
		switch {
		case errors.Is(err, format.ErrConflict):
			http.Error(w, "publication conflict", http.StatusConflict)
		case errors.Is(err, format.ErrPolicyRejected):
			http.Error(w, "publication rejected", http.StatusBadRequest)
		case errors.Is(err, format.ErrUploadLimit):
			http.Error(w, "upload limit exceeded", http.StatusRequestEntityTooLarge)
		default:
			http.Error(w, "publication failed", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
}

var _ format.RetentionGrouping = textFormat{}
var _ format.WireProtocol = textFormat{}
