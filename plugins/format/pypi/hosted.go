package pypi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	// fileContentType is the stored content type of an uploaded distribution.
	fileContentType        = "application/octet-stream"
	maximumMultipartFields = 64
	maximumFieldBytes      = 10 << 20
)

// WireAction claims, on hosted repositories only, the `twine upload` POST (at
// the repository root or a `legacy/` suffix) and the simple-index GETs.
// Distribution downloads (packages/…) keep riding the generic asset pipeline,
// so proxy and group repositories are untouched.
func (Format) WireAction(
	repository format.Repository,
	method string,
	requestPath string,
	_ url.Values,
) (string, bool) {
	if repository.Type != "hosted" {
		return "", false
	}
	switch method {
	case http.MethodPost:
		if trimmed := strings.Trim(requestPath, "/"); trimmed == "" || trimmed == "legacy" {
			return "write", true
		}
	case http.MethodGet, http.MethodHead:
		if info, ok := parsePath(requestPath); ok && (info.kind == kindRoot || info.kind == kindProject) {
			return "read", true
		}
	}
	return "", false
}

// ServeWire serves a hosted upload or a synthesized simple-index page.
func (Format) ServeWire(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	requestPath string,
	tools format.WireTools,
) {
	if r.Method == http.MethodPost {
		uploadFile(w, r, tools)
		return
	}
	info, ok := parsePath(requestPath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch info.kind {
	case kindRoot:
		serveRootIndex(w, r, repository, tools)
	case kindProject:
		serveProjectPage(w, r, repository, tools, info.project)
	default:
		http.NotFound(w, r)
	}
}

// uploadFile stores one distribution from a `twine upload` multipart body.
func uploadFile(w http.ResponseWriter, r *http.Request, tools format.WireTools) {
	limit := tools.MaxUploadBytes()
	if limit <= 0 {
		limit = 1
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart upload")
		return
	}
	upload := &multipartUpload{reader: reader, fields: make(map[string]string)}
	var file *multipart.Part
	for file == nil {
		part, nextErr := upload.nextPart()
		if nextErr != nil {
			if nextErr == io.EOF {
				writeError(w, http.StatusBadRequest, "upload has no content file")
			} else {
				writeMultipartError(w, nextErr)
			}
			return
		}
		if part != nil {
			file = part
		}
	}
	filename := file.FileName()
	if !validFilename(filename) {
		writeError(w, http.StatusBadRequest, "invalid distribution filename")
		return
	}
	filenameProject, _, recognizable := parseDistributionFilename(filename)
	if !recognizable || !validProjectName(filenameProject) {
		writeError(w, http.StatusBadRequest, "filename is not a supported distribution")
		return
	}
	assetPath := packagesPrefix + filenameProject + "/" + filename
	content := &validatedContent{part: file, upload: upload, filenameProject: filenameProject, hash: sha256.New()}
	if _, err := tools.StoreAsset(r.Context(), assetPath, fileContentType, content); err != nil {
		var validation *multipartValidationError
		if errors.As(err, &validation) {
			writeError(w, validation.status, validation.message)
		} else {
			var sizeError *http.MaxBytesError
			if errors.As(err, &sizeError) {
				writeError(w, http.StatusRequestEntityTooLarge, "upload exceeds the configured limit")
			} else {
				writePublicationError(w, r, tools, err)
			}
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

// multipartUpload keeps metadata bounded even when it follows the distribution.
// The host's StoreAsset stages the content in its owned, locked upload area;
// returning a validation error from the reader prevents publication.
type multipartUpload struct {
	reader     *multipart.Reader
	fields     map[string]string
	fieldCount int
	fieldBytes int64
}

type multipartValidationError struct {
	status  int
	message string
}

func (err *multipartValidationError) Error() string { return err.message }

func invalidMultipart(err error) error {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		return err
	}
	return &multipartValidationError{http.StatusBadRequest, "invalid multipart upload"}
}

func writeMultipartError(w http.ResponseWriter, err error) {
	var validation *multipartValidationError
	if errors.As(err, &validation) {
		writeError(w, validation.status, validation.message)
		return
	}
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		writeError(w, http.StatusRequestEntityTooLarge, "upload exceeds the configured limit")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid multipart upload")
}

// nextPart consumes a bounded field, or returns the sole distribution part.
func (upload *multipartUpload) nextPart() (*multipart.Part, error) {
	part, err := upload.reader.NextPart()
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, invalidMultipart(err)
	}
	if part.FileName() != "" {
		if part.FormName() != "content" {
			return nil, &multipartValidationError{http.StatusBadRequest, "upload contains an unexpected file part"}
		}
		return part, nil
	}
	upload.fieldCount++
	if upload.fieldCount > maximumMultipartFields {
		return nil, &multipartValidationError{http.StatusRequestEntityTooLarge, "upload contains too many multipart fields"}
	}
	name := part.FormName()
	remaining := int64(maximumFieldBytes) - upload.fieldBytes - int64(len(name))
	if remaining < 0 {
		return nil, &multipartValidationError{http.StatusRequestEntityTooLarge, "upload contains too many multipart fields"}
	}
	value, err := io.ReadAll(io.LimitReader(part, remaining+1))
	if err != nil {
		return nil, invalidMultipart(err)
	}
	if int64(len(value)) > remaining {
		return nil, &multipartValidationError{http.StatusRequestEntityTooLarge, "upload contains too many multipart fields"}
	}
	upload.fieldBytes += int64(len(name) + len(value))
	if name == "name" || name == "sha256_digest" {
		if _, found := upload.fields[name]; !found {
			upload.fields[name] = string(value)
		}
	}
	return nil, nil
}

type validatedContent struct {
	part            *multipart.Part
	upload          *multipartUpload
	filenameProject string
	hash            hash.Hash
	done            bool
}

func (content *validatedContent) Read(buffer []byte) (int, error) {
	if content.done {
		return 0, io.EOF
	}
	n, err := content.part.Read(buffer)
	if n > 0 {
		_, _ = content.hash.Write(buffer[:n])
	}
	if err == nil {
		return n, nil
	}
	if err != io.EOF {
		return n, invalidMultipart(err)
	}
	for {
		part, nextErr := content.upload.nextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return n, nextErr
		}
		if part != nil {
			return n, &multipartValidationError{http.StatusRequestEntityTooLarge, "upload contains too many multipart fields"}
		}
	}
	project := content.upload.fields["name"]
	if project == "" {
		project = content.filenameProject
	}
	project = normalizeProjectName(project)
	if !validProjectName(project) || project != content.filenameProject {
		return n, &multipartValidationError{http.StatusBadRequest, "project name does not match distribution filename"}
	}
	if advertised := content.upload.fields["sha256_digest"]; advertised != "" &&
		!strings.EqualFold(advertised, hex.EncodeToString(content.hash.Sum(nil))) {
		return n, &multipartValidationError{http.StatusBadRequest, "sha256_digest does not match distribution content"}
	}
	content.done = true
	return n, io.EOF
}

func writePublicationError(w http.ResponseWriter, r *http.Request, tools format.WireTools, err error) {
	switch {
	case errors.Is(err, format.ErrConflict):
		writeError(w, http.StatusConflict, "distribution already exists")
	case errors.Is(err, format.ErrPolicyRejected):
		writeError(w, http.StatusForbidden, "publication rejected by repository policy")
	case errors.Is(err, format.ErrUploadLimit):
		writeError(w, http.StatusRequestEntityTooLarge, "publication exceeds upload limit")
	default:
		writeInternalError(w, r, tools, "store distribution", err)
	}
}

// serveProjectPage synthesizes a project's simple-index page from the files
// stored under its packages/ prefix.
func serveProjectPage(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	tools format.WireTools,
	project string,
) {
	ctx := r.Context()
	normalized := normalizeProjectName(project)
	repositoryURL := repositoryURL(r, repository.Name)
	page := &simplePage{kind: kindProject, name: normalized, asJSON: wantsJSON(r), seen: map[string]struct{}{}}
	budget := newOutputBudget()
	err := tools.VisitAssetPaths(ctx, packagesPrefix+normalized+"/", func(assetPath string) (bool, error) {
		info, ok := parsePath(assetPath)
		if !ok || info.kind != kindHostedFile {
			return true, nil
		}
		asset, found, err := tools.StatAsset(ctx, assetPath)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		if _, duplicate := page.seen[info.filename]; duplicate {
			return true, nil
		}
		if err := budget.charge(int64(len(repositoryURL) + 1 + len(assetPath))); err != nil {
			return false, err
		}
		entry := map[string]any{
			"filename": info.filename,
			"url":      repositoryURL + "/" + assetPath,
			"size":     asset.Size,
		}
		if digest, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok {
			entry["hashes"] = map[string]any{"sha256": digest}
		}
		page.add(entry)
		return true, nil
	})
	if err != nil {
		writeInternalError(w, r, tools, "list distributions", err)
		return
	}
	if len(page.entries) == 0 {
		http.NotFound(w, r)
		return
	}
	renderPage(w, r, tools, page)
}

// serveRootIndex synthesizes the root index listing every project that has at
// least one stored distribution.
func serveRootIndex(
	w http.ResponseWriter,
	r *http.Request,
	repository format.Repository,
	tools format.WireTools,
) {
	ctx := r.Context()
	repositoryURL := repositoryURL(r, repository.Name)
	page := &simplePage{kind: kindRoot, asJSON: wantsJSON(r), seen: map[string]struct{}{}}
	budget := newOutputBudget()
	err := tools.VisitAssetPaths(ctx, packagesPrefix, func(assetPath string) (bool, error) {
		info, ok := parsePath(assetPath)
		if !ok || info.kind != kindHostedFile {
			return true, nil
		}
		if _, duplicate := page.seen[normalizeProjectName(info.project)]; duplicate {
			return true, nil
		}
		if err := budget.charge(int64(len(repositoryURL) + len("/simple//") + len(info.project))); err != nil {
			return false, err
		}
		page.add(map[string]any{
			"name": info.project,
			"url":  repositoryURL + "/" + simplePrefix + "/" + info.project + "/",
		})
		return true, nil
	})
	if err != nil {
		writeInternalError(w, r, tools, "list projects", err)
		return
	}
	renderPage(w, r, tools, page)
}

func renderPage(w http.ResponseWriter, r *http.Request, tools format.WireTools, page *simplePage) {
	content, contentType, err := page.render()
	if err != nil {
		writeInternalError(w, r, tools, "render index", err)
		return
	}
	if contentType == htmlContentType && simpleMediaTypeForAccept(strings.Join(r.Header.Values("Accept"), ",")) == vendorHTMLContentType {
		contentType = vendorHTMLContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Add("Vary", "Accept")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(content)
	}
}

// wantsJSON reports whether the client accepts the PEP 691 JSON simple index.
func wantsJSON(r *http.Request) bool {
	return acceptWantsJSON(strings.Join(r.Header.Values("Accept"), ","))
}

const vendorHTMLContentType = "application/vnd.pypi.simple.v1+html"

func acceptWantsJSON(accept string) bool {
	return simpleMediaTypeForAccept(accept) == jsonContentType
}

// simpleMediaTypeForAccept selects among the three serializations this plugin
// can emit. A more specific media range overrides a wildcard for that type.
func simpleMediaTypeForAccept(accept string) string {
	if strings.TrimSpace(accept) == "" {
		return htmlContentType
	}
	type candidate struct {
		media       string
		quality     float64
		specificity int
	}
	choices := []candidate{{jsonContentType, -1, -1}, {vendorHTMLContentType, -1, -1}, {htmlContentType, -1, -1}}
	for _, item := range strings.Split(accept, ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		quality := 1.0
		if raw, ok := parameters["q"]; ok {
			quality, err = strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(quality) || math.IsInf(quality, 0) || quality < 0 || quality > 1 {
				continue
			}
		}
		for index := range choices {
			specificity := -1
			switch mediaType {
			case "*/*":
				specificity = 0
			case "application/*":
				if index < 2 {
					specificity = 1
				}
			case "text/*":
				if index == 2 {
					specificity = 1
				}
			case "application/vnd.pypi.simple.v1+json", "application/vnd.pypi.simple.latest+json":
				if index == 0 {
					specificity = 2
				}
			case "application/vnd.pypi.simple.v1+html", "application/vnd.pypi.simple.latest+html":
				if index == 1 {
					specificity = 2
				}
			case "text/html":
				if index == 2 {
					specificity = 2
				}
			}
			if specificity < 0 {
				continue
			}
			if specificity > choices[index].specificity || specificity == choices[index].specificity && quality > choices[index].quality {
				choices[index].quality = quality
				choices[index].specificity = specificity
			}
		}
	}
	// Prefer an explicitly selected representation. For an undifferentiated
	// wildcard, retain legacy HTML as the default.
	best := -1
	for index, current := range choices {
		if current.quality <= 0 {
			continue
		}
		if best < 0 {
			best = index
			continue
		}
		previous := choices[best]
		if current.quality != previous.quality {
			if current.quality > previous.quality {
				best = index
			}
			continue
		}
		if current.specificity != previous.specificity {
			if current.specificity > previous.specificity {
				best = index
			}
			continue
		}
		if current.specificity == 0 && index == 2 || current.specificity > 0 && index < best {
			best = index
		}
	}
	if best < 0 {
		return htmlContentType
	}
	return choices[best].media
}

// repositoryURL is the absolute URL of the repository root as the request
// reached it, so synthesized links work under every hostname the instance is
// served on.
func repositoryURL(r *http.Request, repositoryName string) string {
	scheme := "http"
	if r.TLS != nil || forwardedHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "suxen"
	}
	return scheme + "://" + host + "/repository/" + url.PathEscape(repositoryName)
}

func forwardedHTTPS(r *http.Request) bool {
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	if comma := strings.IndexByte(proto, ','); comma >= 0 {
		proto = strings.TrimSpace(proto[:comma])
	}
	return proto == "https"
}

func writeError(w http.ResponseWriter, status int, message string) {
	http.Error(w, message, status)
}

func writeInternalError(w http.ResponseWriter, r *http.Request, tools format.WireTools, operation string, err error) {
	format.ReportWireError(tools, r.Context(), operation, err)
	writeError(w, http.StatusInternalServerError, "registry temporarily unavailable")
}
