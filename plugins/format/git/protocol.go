package git

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/suxen-project/suxen/spi/format"
)

const (
	contentTypeUploadPackAdvertisement  = "application/x-git-upload-pack-advertisement"
	contentTypeReceivePackAdvertisement = "application/x-git-receive-pack-advertisement"
	contentTypeUploadPackRequest        = "application/x-git-upload-pack-request"
	contentTypeUploadPackResult         = "application/x-git-upload-pack-result"
	contentTypePackfile                 = "application/x-git-packfile"

	// maxRequestBodyBytes bounds a client's upload-pack request; a fetch with
	// thousands of haves stays far below it.
	maxRequestBodyBytes = 1 << 20
	agentCapability     = "agent=suxen"
)

// protocolServer answers the smart HTTP protocol (version 2) for one
// repository route.
type protocolServer struct {
	tools      format.WireTools
	repository format.Repository
	route      route
}

// requestsProtocolV2 reports whether the client negotiated protocol version
// 2 through the Git-Protocol header, whose value is a colon-separated list
// of key=value pairs.
func requestsProtocolV2(r *http.Request) bool {
	for _, header := range r.Header.Values("Git-Protocol") {
		for _, entry := range strings.Split(header, ":") {
			if strings.TrimSpace(entry) == "version=2" {
				return true
			}
		}
	}
	return false
}

func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	w.Header().Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	w.Header().Set("Pragma", "no-cache")
}

// writeProtocolError answers with an ERR packet, which git shows the user as
// "remote error: ...". The HTTP status stays 200 because git discards the
// body of any other status.
func writeProtocolError(w http.ResponseWriter, contentType string, message string) {
	setNoCache(w)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_ = writePktLine(w, "ERR "+message)
}

func (s protocolServer) serveInfoRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Query().Get("service") {
	case serviceUploadPack:
	case serviceReceivePack:
		writeProtocolError(w, contentTypeReceivePackAdvertisement, readOnlyMessage)
		return
	default:
		http.Error(w, "the dumb git HTTP protocol is not supported", http.StatusBadRequest)
		return
	}
	if !requestsProtocolV2(r) {
		writeProtocolError(w, contentTypeUploadPackAdvertisement, protocolV2Message)
		return
	}
	setNoCache(w)
	w.Header().Set("Content-Type", contentTypeUploadPackAdvertisement)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	var body pktBuffer
	body.line("# service=" + serviceUploadPack).flush()
	body.line("version 2").
		line(agentCapability).
		line("ls-refs").
		line("fetch=shallow").
		line("object-format=sha1").
		flush()
	_, _ = w.Write(body.Bytes())
}

const (
	readOnlyMessage   = "this repository serves read-only snapshots of its upstream; pushes are not accepted"
	protocolV2Message = "this repository serves git protocol version 2 only; " +
		"use git 2.18 or newer (or set protocol.version=2)"
	singleWantMessage = "this repository serves one snapshot per fetch; " +
		"clone with --depth 1, --single-branch, or --branch <ref>"
)

func (s protocolServer) serveReceivePack(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, readOnlyMessage, http.StatusForbidden)
}

func (s protocolServer) serveUploadPack(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requestsProtocolV2(r) {
		writeProtocolError(w, contentTypeUploadPackResult, protocolV2Message)
		return
	}
	request, err := readCommandRequest(r)
	if err != nil {
		writeProtocolError(w, contentTypeUploadPackResult, "invalid request: "+err.Error())
		return
	}
	switch request.command {
	case "ls-refs":
		s.serveLsRefs(w, r.Context(), request)
	case "fetch":
		s.serveFetch(w, r.Context(), request)
	default:
		writeProtocolError(
			w,
			contentTypeUploadPackResult,
			fmt.Sprintf("unsupported command %q", request.command),
		)
	}
}

// commandRequest is one protocol v2 command: the command line, capability
// lines up to the delimiter, and argument lines up to the flush.
type commandRequest struct {
	command      string
	capabilities []string
	args         []string
}

func readCommandRequest(r *http.Request) (commandRequest, error) {
	var body io.Reader = http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		decompressed, err := gzip.NewReader(body)
		if err != nil {
			return commandRequest{}, fmt.Errorf("decompress body: %w", err)
		}
		defer decompressed.Close()
		body = io.LimitReader(decompressed, maxRequestBodyBytes)
	}
	return parseCommandRequest(bufio.NewReader(body))
}

func parseCommandRequest(r io.Reader) (commandRequest, error) {
	var request commandRequest
	kind, line, err := readPktLine(r)
	if err != nil {
		return request, fmt.Errorf("read command: %w", err)
	}
	if kind != pktData || !strings.HasPrefix(line, "command=") {
		return request, errors.New("request does not start with a command")
	}
	request.command = strings.TrimPrefix(line, "command=")

	inArguments := false
	for {
		kind, line, err := readPktLine(r)
		if err != nil {
			return request, fmt.Errorf("read request: %w", err)
		}
		switch kind {
		case pktFlush:
			return request, nil
		case pktDelim:
			inArguments = true
		case pktData:
			if inArguments {
				request.args = append(request.args, line)
			} else {
				request.capabilities = append(request.capabilities, line)
			}
		case pktResponseEnd:
			return request, errors.New("unexpected response-end packet in request")
		}
	}
}

func (s protocolServer) serveLsRefs(w http.ResponseWriter, ctx context.Context, request commandRequest) {
	var prefixes []string
	peel, symrefs := false, false
	for _, arg := range request.args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(arg, "ref-prefix "))
		}
	}

	refs, err := s.loadRefs(ctx)
	if err != nil {
		s.writeBackendError(w, ctx, "load refs", err)
		return
	}
	setNoCache(w)
	w.Header().Set("Content-Type", contentTypeUploadPackResult)
	w.WriteHeader(http.StatusOK)
	var body pktBuffer
	for _, ref := range refs {
		if !matchesPrefixes(ref.name, prefixes) {
			continue
		}
		line := ref.oid + " " + ref.name
		if symrefs && ref.symrefTarget != "" {
			line += " symref-target:" + ref.symrefTarget
		}
		if peel && ref.peeled != "" {
			line += " peeled:" + ref.peeled
		}
		body.line(line)
	}
	body.flush()
	_, _ = w.Write(body.Bytes())
}

func matchesPrefixes(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// loadRefs returns the cached ref list, refreshing it from the upstream when
// it is missing or older than the proxy TTL.
func (s protocolServer) loadRefs(ctx context.Context) ([]ref, error) {
	assetPath := refsPath(s.route.key)
	cached, found, err := s.tools.StatAsset(ctx, assetPath)
	if err != nil {
		return nil, fmt.Errorf("read cached refs: %w", err)
	}
	if found && !stale(cached.ValidatedAt, s.tools.ProxyTTL(), time.Now()) {
		content, _, found, err := s.tools.OpenMetadataAsset(ctx, assetPath)
		if err != nil {
			return nil, fmt.Errorf("open cached refs: %w", err)
		}
		if found {
			defer content.Close()
			return parseRefs(io.LimitReader(content, maxRefsBodyBytes))
		}
	}
	return s.refreshRefs(ctx)
}

// stale mirrors the host's proxy revalidation rule: a zero TTL revalidates on
// every request.
func stale(updatedAt time.Time, ttl time.Duration, now time.Time) bool {
	return ttl == 0 || now.Sub(updatedAt) >= ttl
}

func (s protocolServer) serveFetch(w http.ResponseWriter, ctx context.Context, request commandRequest) {
	var wants []string
	done := false
	for _, arg := range request.args {
		switch {
		case strings.HasPrefix(arg, "want "):
			oid := strings.TrimPrefix(arg, "want ")
			if !validObjectID(oid) {
				writeProtocolError(w, contentTypeUploadPackResult, fmt.Sprintf("invalid want %q", oid))
				return
			}
			if !containsString(wants, oid) {
				wants = append(wants, oid)
			}
		case arg == "done":
			done = true
		}
	}
	switch {
	case len(wants) == 0:
		writeProtocolError(w, contentTypeUploadPackResult, "fetch request carries no want")
		return
	case len(wants) > 1:
		writeProtocolError(w, contentTypeUploadPackResult, singleWantMessage)
		return
	}
	want := wants[0]
	pack, shallowCommit, err := s.openSnapshot(ctx, want)
	if err != nil {
		s.writeBackendError(w, ctx, "open snapshot", err)
		return
	}
	defer pack.Close()

	setNoCache(w)
	w.Header().Set("Content-Type", contentTypeUploadPackResult)
	w.WriteHeader(http.StatusOK)
	out := bufio.NewWriterSize(w, maxPktLength)
	// A client that still negotiates (haves sent, no done) accepts the pack
	// as soon as the server declares itself ready; declaring it without any
	// ACK just means the whole snapshot streams, which is the contract.
	if !done {
		_ = writePktLine(out, "acknowledgments")
		_ = writePktLine(out, "ready")
		_ = writeDelim(out)
	}
	// The upstream's shallow boundary is a commit, even when the requested
	// object is an annotated tag. A complete root snapshot needs no boundary.
	if shallowCommit != "" {
		_ = writePktLine(out, "shallow-info")
		_ = writePktLine(out, "shallow "+shallowCommit)
		_ = writeDelim(out)
	}
	_ = writePktLine(out, "packfile")
	if _, err := io.Copy(sideBandWriter{w: out}, pack); err != nil {
		format.ReportWireError(s.tools, ctx, "stream snapshot", err)
		_ = writeSideBand(out, sideBandError, []byte("snapshot stream failed\n"))
		_ = out.Flush()
		return
	}
	_ = writeFlush(out)
	_ = out.Flush()
}

// shallowCommit resolves an annotated tag through the cached ref list. It is
// a fallback when an upstream response does not declare a shallow commit.
func (s protocolServer) shallowCommit(ctx context.Context, want string) (string, error) {
	content, _, found, err := s.tools.OpenMetadataAsset(ctx, refsPath(s.route.key))
	if err != nil {
		return "", fmt.Errorf("open cached refs: %w", err)
	}
	if !found {
		return want, nil
	}
	defer content.Close()
	refs, err := parseRefs(io.LimitReader(content, maxRefsBodyBytes))
	if err != nil {
		return "", fmt.Errorf("parse cached refs: %w", err)
	}
	for _, ref := range refs {
		if ref.oid == want && ref.peeled != "" {
			if !validObjectID(ref.peeled) {
				return "", errors.New("cached ref has an invalid peeled object id")
			}
			return ref.peeled, nil
		}
	}
	return want, nil
}

func (s protocolServer) writeBackendError(w http.ResponseWriter, ctx context.Context, operation string, err error) {
	// These public categories are actionable to native clients. Other causes,
	// including upstream response bodies, may contain sensitive details.
	switch {
	case errors.Is(err, errUpstreamAccess):
		writeProtocolError(w, contentTypeUploadPackResult, "upstream rejected repository access; check proxy credentials")
	case errors.Is(err, errUpstreamMissing):
		writeProtocolError(w, contentTypeUploadPackResult, "upstream repository not found")
	case errors.Is(err, errUpstreamProtocol):
		writeProtocolError(w, contentTypeUploadPackResult, "upstream does not support git protocol v2 and shallow fetch")
	case errors.Is(err, format.ErrPolicyRejected):
		writeProtocolError(w, contentTypeUploadPackResult, "snapshot rejected by repository policy")
	case errors.Is(err, format.ErrUploadLimit):
		writeProtocolError(w, contentTypeUploadPackResult, "snapshot exceeds upload limit")
	case errors.Is(err, format.ErrConflict):
		writeProtocolError(w, contentTypeUploadPackResult, "snapshot conflict")
	default:
		format.ReportWireError(s.tools, ctx, operation, err)
		writeProtocolError(w, contentTypeUploadPackResult, "snapshot temporarily unavailable")
	}
}

// openSnapshot streams the stored pack with its immutable shallow boundary.
// Older cache rows without that parameter derive it from the ref list.
func (s protocolServer) openSnapshot(ctx context.Context, want string) (io.ReadCloser, string, error) {
	assetPath := snapshotPath(s.route.key, want)
	content, asset, found, err := s.tools.OpenAsset(ctx, assetPath)
	if err != nil {
		return nil, "", fmt.Errorf("open cached snapshot: %w", err)
	}
	if found {
		boundary, known, err := snapshotBoundary(asset.ContentType)
		if err != nil {
			content.Close()
			return nil, "", err
		}
		if !known {
			boundary, err = s.shallowCommit(ctx, want)
			if err != nil {
				content.Close()
				return nil, "", err
			}
		}
		return content, boundary, nil
	}
	if err := s.fetchSnapshot(ctx, want); err != nil {
		return nil, "", err
	}
	content, asset, found, err = s.tools.OpenAsset(ctx, assetPath)
	if err != nil {
		return nil, "", fmt.Errorf("open stored snapshot: %w", err)
	}
	if !found {
		return nil, "", errors.New("snapshot vanished after it was stored")
	}
	boundary, known, err := snapshotBoundary(asset.ContentType)
	if err != nil || !known {
		content.Close()
		return nil, "", errors.New("stored snapshot has no valid shallow boundary")
	}
	return content, boundary, nil
}

func snapshotBoundary(contentType string) (string, bool, error) {
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != contentTypePackfile {
		return "", false, errors.New("stored snapshot has invalid content type")
	}
	boundary := parameters["shallow"]
	if boundary != "" {
		if !validObjectID(boundary) || parameters["snapshot"] != "" {
			return "", false, errors.New("stored snapshot has invalid shallow boundary")
		}
		return boundary, true, nil
	}
	if parameters["snapshot"] == "complete" {
		return "", true, nil
	}
	return "", false, nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
