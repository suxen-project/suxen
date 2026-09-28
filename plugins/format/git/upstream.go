package git

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
)

// maxRefsBodyBytes bounds an upstream ref list; the largest public
// repositories advertise a few megabytes of refs.
const maxRefsBodyBytes = 8 << 20

var (
	errUpstreamAccess   = errors.New("upstream rejected repository access")
	errUpstreamMissing  = errors.New("upstream repository not found")
	errUpstreamProtocol = errors.New("upstream does not support required git protocol features")
)

// ref is one advertised reference.
type ref struct {
	oid          string
	name         string
	symrefTarget string
	peeled       string
}

// parseRefs decodes an ls-refs response body as stored in the ref list
// asset. Lines whose object id is not a SHA-1 (an unborn HEAD) are skipped.
func parseRefs(r io.Reader) ([]ref, error) {
	var refs []ref
	for {
		kind, line, err := readPktLine(r)
		if errors.Is(err, io.EOF) || kind == pktFlush {
			return refs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse ref list: %w", err)
		}
		if kind != pktData {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !validObjectID(fields[0]) {
			continue
		}
		entry := ref{oid: fields[0], name: fields[1]}
		for _, attribute := range fields[2:] {
			switch {
			case strings.HasPrefix(attribute, "symref-target:"):
				entry.symrefTarget = strings.TrimPrefix(attribute, "symref-target:")
			case strings.HasPrefix(attribute, "peeled:"):
				entry.peeled = strings.TrimPrefix(attribute, "peeled:")
			}
		}
		refs = append(refs, entry)
	}
}

func upstreamHeader(contentType string) http.Header {
	header := http.Header{}
	header.Set("Git-Protocol", "version=2")
	if contentType != "" {
		header.Set("Content-Type", contentType)
		header.Set("Accept", contentTypeUploadPackResult)
	}
	return header
}

// refreshRefs fetches the upstream's ref list with protocol v2 and stores the
// ls-refs response verbatim as the repository's ref list asset.
func (s protocolServer) refreshRefs(ctx context.Context) ([]ref, error) {
	if err := s.checkUpstreamCapabilities(ctx); err != nil {
		return nil, err
	}
	var request pktBuffer
	request.line("command=ls-refs").line(agentCapability).line("object-format=sha1").delim().
		line("peel").line("symrefs").flush()
	response, err := s.tools.Upstream(
		ctx,
		http.MethodPost,
		s.route.repo+"/"+serviceUploadPack,
		upstreamHeader(contentTypeUploadPackRequest),
		bytes.NewReader(request.Bytes()),
	)
	if err != nil {
		return nil, fmt.Errorf("upstream ls-refs: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, upstreamStatusError("ls-refs", response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRefsBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("upstream ls-refs: %w", err)
	}
	if len(body) > maxRefsBodyBytes {
		return nil, errors.New("upstream ref list exceeds the size limit")
	}
	refs, err := parseRefs(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upstream ls-refs: %w", err)
	}
	if _, err := s.tools.StoreMetadataAsset(
		ctx,
		refsPath(s.route.key),
		contentTypeUploadPackResult,
		bytes.NewReader(body),
	); err != nil {
		return nil, fmt.Errorf("store ref list: %w", err)
	}
	return refs, nil
}

// checkUpstreamCapabilities reads the upstream's capability advertisement
// and requires protocol v2 with ls-refs and shallow fetch, the two commands
// the snapshot model is built on.
func (s protocolServer) checkUpstreamCapabilities(ctx context.Context) error {
	response, err := s.tools.Upstream(
		ctx,
		http.MethodGet,
		s.route.repo+"/"+endpointInfoRefs+"?service="+serviceUploadPack,
		upstreamHeader(""),
		nil,
	)
	if err != nil {
		return fmt.Errorf("upstream advertisement: %w", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("upstream advertisement: %w", errUpstreamAccess)
	case http.StatusNotFound:
		return errUpstreamMissing
	default:
		return upstreamStatusError("advertisement", response)
	}
	if mediaType := response.Header.Get("Content-Type"); !strings.HasPrefix(mediaType, contentTypeUploadPackAdvertisement) {
		return errUpstreamProtocol
	}
	return parseAdvertisement(io.LimitReader(response.Body, maxRefsBodyBytes))
}

func parseAdvertisement(r io.Reader) error {
	sawVersion, sawLsRefs, sawShallowFetch := false, false, false
	for {
		kind, line, err := readPktLine(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("upstream advertisement: %w", err)
		}
		switch {
		case kind != pktData:
			if sawVersion {
				// The capability list ends at the first flush after the
				// version line; the service header's flush precedes it.
				return capabilityResult(sawLsRefs, sawShallowFetch)
			}
		case strings.HasPrefix(line, "# service="):
		case strings.HasPrefix(line, "ERR "):
			return errors.New("upstream error: " + strings.TrimPrefix(line, "ERR "))
		case line == "version 2":
			sawVersion = true
		case !sawVersion:
			return errUpstreamProtocol
		case line == "ls-refs" || strings.HasPrefix(line, "ls-refs="):
			sawLsRefs = true
		case strings.HasPrefix(line, "fetch"):
			features := strings.Fields(strings.TrimPrefix(line, "fetch="))
			sawShallowFetch = containsString(features, "shallow")
		}
	}
	if !sawVersion {
		return errUpstreamProtocol
	}
	return capabilityResult(sawLsRefs, sawShallowFetch)
}

func capabilityResult(lsRefs, shallowFetch bool) error {
	if !lsRefs || !shallowFetch {
		return errUpstreamProtocol
	}
	return nil
}

// fetchSnapshot performs a depth-1 fetch of one wanted object and stores the
// pack with its shallow commit boundary in one transaction.
func (s protocolServer) fetchSnapshot(ctx context.Context, want string) error {
	var request pktBuffer
	request.line("command=fetch").line(agentCapability).line("object-format=sha1").delim().
		line("want " + want).line("deepen 1").line("no-progress").line("done").flush()
	response, err := s.tools.Upstream(
		ctx,
		http.MethodPost,
		s.route.repo+"/"+serviceUploadPack,
		upstreamHeader(contentTypeUploadPackRequest),
		bytes.NewReader(request.Bytes()),
	)
	if err != nil {
		return fmt.Errorf("upstream fetch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return upstreamStatusError("fetch", response)
	}
	shallowCommit, err := readPackfileSection(response.Body)
	if err != nil {
		return err
	}

	// Demultiplex the side-band packfile section into a pipe so the pack is
	// verified and staged as it streams, without buffering it in memory.
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(demuxPackfileData(response.Body, writer))
	}()
	verifier := newPackVerifier(reader)
	contentType := contentTypePackfile + "; snapshot=complete"
	if shallowCommit != "" {
		contentType = contentTypePackfile + "; shallow=" + shallowCommit
	}
	if _, err := s.tools.StoreAsset(ctx, snapshotPath(s.route.key, want), contentType, verifier); err != nil {
		_ = reader.CloseWithError(err)
		return fmt.Errorf("store snapshot %s: %w", want, err)
	}
	return nil
}

func upstreamStatusError(operation string, response *http.Response) error {
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("upstream %s: %w", operation, errUpstreamAccess)
	case http.StatusNotFound:
		return fmt.Errorf("upstream %s: %w", operation, errUpstreamMissing)
	default:
		return fmt.Errorf("upstream %s returned status %d", operation, response.StatusCode)
	}
}

// demuxPackfile skips the fetch response sections before the packfile and
// writes the data channel of the packfile section to out.
func demuxPackfile(r io.Reader, out io.Writer) error {
	if _, err := readPackfileSection(r); err != nil {
		return err
	}
	return demuxPackfileData(r, out)
}

// readPackfileSection consumes response sections up to the packfile marker.
// The upstream's shallow line names the commit boundary even when the wanted
// object is an annotated tag. An empty result means the pack needs no shallow
// boundary, as with a root commit.
func readPackfileSection(r io.Reader) (string, error) {
	var shallowCommit string
	for {
		kind, data, err := readPkt(r)
		if err != nil {
			return "", fmt.Errorf("upstream fetch response: %w", err)
		}
		switch kind {
		case pktData:
			line := strings.TrimSuffix(string(data), "\n")
			if strings.HasPrefix(line, "ERR ") {
				return "", errors.New("upstream error: " + strings.TrimPrefix(line, "ERR "))
			}
			if line == "packfile" {
				return shallowCommit, nil
			}
			if oid, found := strings.CutPrefix(line, "shallow "); found {
				if !validObjectID(oid) || shallowCommit != "" && shallowCommit != oid {
					return "", errors.New("upstream fetch response has invalid shallow boundary")
				}
				shallowCommit = oid
			}
		}
	}
}

func demuxPackfileData(r io.Reader, out io.Writer) error {
	for {
		kind, data, err := readPkt(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("upstream fetch response: %w", err)
		}
		switch kind {
		case pktFlush:
			return nil
		case pktData:
			if len(data) == 0 {
				continue
			}
			switch data[0] {
			case sideBandData:
				if _, err := out.Write(data[1:]); err != nil {
					return err
				}
			case sideBandProgress:
			case sideBandError:
				return errors.New("upstream error: " + strings.TrimSpace(string(data[1:])))
			default:
				return fmt.Errorf("upstream fetch response: unknown side-band %d", data[0])
			}
		}
	}
}

// packVerifier checks a packfile as it streams: the header must announce a
// version 2 pack with at least one object and the trailing SHA-1 must match
// the preceding bytes. A truncated or corrupted upstream pack is rejected
// before it is stored.
type packVerifier struct {
	source  io.Reader
	hash    hash.Hash
	header  []byte
	tail    []byte
	total   int64
	checked bool
}

const (
	packHeaderLength  = 12
	packTrailerLength = sha1.Size
)

func newPackVerifier(source io.Reader) *packVerifier {
	return &packVerifier{source: source, hash: sha1.New()}
}

func (v *packVerifier) Read(p []byte) (int, error) {
	n, err := v.source.Read(p)
	if n > 0 {
		if verifyErr := v.consume(p[:n]); verifyErr != nil {
			return 0, verifyErr
		}
	}
	if errors.Is(err, io.EOF) {
		if finishErr := v.finish(); finishErr != nil {
			return 0, finishErr
		}
	}
	return n, err
}

func (v *packVerifier) consume(data []byte) error {
	v.total += int64(len(data))
	if !v.checked {
		v.header = append(v.header, data...)
		if len(v.header) < packHeaderLength {
			return nil
		}
		if err := checkPackHeader(v.header[:packHeaderLength]); err != nil {
			return err
		}
		v.checked = true
		data = v.header
		v.header = nil
	}
	combined := append(v.tail, data...)
	if len(combined) > packTrailerLength {
		v.hash.Write(combined[:len(combined)-packTrailerLength])
		combined = combined[len(combined)-packTrailerLength:]
	}
	v.tail = append([]byte(nil), combined...)
	return nil
}

func (v *packVerifier) finish() error {
	if v.total < packHeaderLength+packTrailerLength {
		return errors.New("upstream pack is truncated")
	}
	if !bytes.Equal(v.hash.Sum(nil), v.tail) {
		return errors.New("upstream pack checksum mismatch")
	}
	return nil
}

func checkPackHeader(header []byte) error {
	if string(header[:4]) != "PACK" {
		return errors.New("upstream response is not a packfile")
	}
	if version := binary.BigEndian.Uint32(header[4:8]); version != 2 && version != 3 {
		return fmt.Errorf("unsupported pack version %d", version)
	}
	if binary.BigEndian.Uint32(header[8:12]) == 0 {
		return errors.New("upstream pack contains no objects")
	}
	return nil
}
