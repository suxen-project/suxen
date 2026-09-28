package git

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/suxen-project/suxen/spi/format"
)

func TestParseRoute(t *testing.T) {
	cases := []struct {
		path     string
		repo     string
		key      string
		endpoint string
		ok       bool
	}{
		{"acme/widget.git/info/refs", "acme/widget.git", "acme/widget", endpointInfoRefs, true},
		{"acme/widget/info/refs", "acme/widget", "acme/widget", endpointInfoRefs, true},
		{"/acme/widget.git/git-upload-pack", "acme/widget.git", "acme/widget", serviceUploadPack, true},
		{"widget.git/git-receive-pack", "widget.git", "widget", serviceReceivePack, true},
		{"info/refs", "", "", "", false},
		{"acme/../widget.git/info/refs", "", "", "", false},
		{"acme//widget.git/info/refs", "", "", "", false},
		{".git/info/refs", "", "", "", false},
		{"acme/wid?get.git/info/refs", "", "", "", false},
		{"acme/wid%2Fget.git/info/refs", "", "", "", false},
		{"acme/widget.git/objects/info/packs", "", "", "", false},
	}
	for _, tc := range cases {
		got, ok := parseRoute(tc.path)
		if ok != tc.ok {
			t.Fatalf("parseRoute(%q) ok = %v, want %v", tc.path, ok, tc.ok)
		}
		if got.repo != tc.repo || got.key != tc.key || got.endpoint != tc.endpoint {
			t.Fatalf("parseRoute(%q) = %+v", tc.path, got)
		}
	}
}

func TestAssetPathsRoundTrip(t *testing.T) {
	commit := strings.Repeat("ab", 20)
	key, kind, got, ok := parseAssetPath(snapshotPath("acme/widget", commit))
	if !ok || key != "acme/widget" || kind != assetKindSnapshot || got != commit {
		t.Fatalf("snapshot path parsed as %q %q %q %v", key, kind, got, ok)
	}
	key, kind, _, ok = parseAssetPath(refsPath("acme/widget"))
	if !ok || key != "acme/widget" || kind != assetKindRefs {
		t.Fatalf("refs path parsed as %q %q %v", key, kind, ok)
	}
	for _, invalid := range []string{"acme/widget.git/snapshots/notahash.pack", "acme/widget/refs", "x.git/snapshots/"} {
		if _, _, _, ok := parseAssetPath(invalid); ok {
			t.Fatalf("parseAssetPath(%q) accepted", invalid)
		}
	}

	var f Format
	if !f.MutableUpstreamPath(format.Repository{}, refsPath("acme/widget")) {
		t.Fatal("ref list should be mutable")
	}
	if f.MutableUpstreamPath(format.Repository{}, snapshotPath("acme/widget", commit)) {
		t.Fatal("snapshot should be immutable")
	}
	attributes := f.ProjectAttributes(format.Asset{Path: snapshotPath("acme/widget", commit), ContentType: contentTypePackfile})
	if attributes["repository"] != "acme/widget" || attributes["commit"] != commit {
		t.Fatalf("attributes = %v", attributes)
	}
	tag := strings.Repeat("cd", 20)
	attributes = f.ProjectAttributes(format.Asset{
		Path: snapshotPath("acme/widget", tag), ContentType: contentTypePackfile + "; shallow=" + commit,
	})
	if attributes["repository"] != "acme/widget" || attributes["commit"] != commit {
		t.Fatalf("annotated tag attributes = %v", attributes)
	}
	attributes = f.ProjectAttributes(format.Asset{
		Path: snapshotPath("acme/widget", tag), ContentType: contentTypePackfile + "; snapshot=complete",
	})
	if attributes["repository"] != "acme/widget" || attributes["commit"] != nil {
		t.Fatalf("complete root tag attributes = %v", attributes)
	}
	if f.ProjectAttributes(format.Asset{Path: "unrelated"}) != nil {
		t.Fatal("unrelated path projected attributes")
	}
}

func TestWireAction(t *testing.T) {
	var f Format
	repository := format.Repository{Type: "proxy"}
	cases := []struct {
		path    string
		query   string
		action  string
		claimed bool
	}{
		{"acme/widget.git/info/refs", "service=git-upload-pack", "read", true},
		{"acme/widget.git/info/refs", "service=git-receive-pack", "write", true},
		{"acme/widget.git/info/refs", "", "read", true},
		{"acme/widget.git/git-upload-pack", "", "read", true},
		{"acme/widget.git/git-receive-pack", "", "write", true},
		{"acme/widget.git/HEAD", "", "", false},
	}
	for _, tc := range cases {
		query, _ := url.ParseQuery(tc.query)
		action, claimed := f.WireAction(repository, "GET", tc.path, query)
		if action != tc.action || claimed != tc.claimed {
			t.Fatalf("WireAction(%q?%s) = %q %v", tc.path, tc.query, action, claimed)
		}
	}
}

func TestValidateRepository(t *testing.T) {
	var f Format
	if err := f.ValidateRepository(format.Repository{Type: "proxy"}); err != nil {
		t.Fatalf("proxy rejected: %v", err)
	}
	var violation *format.PolicyViolation
	for _, repoType := range []string{"hosted", "group"} {
		err := f.ValidateRepository(format.Repository{Type: repoType})
		if !errors.As(err, &violation) || violation.Code != "git_proxy_only" {
			t.Fatalf("%s: err = %v", repoType, err)
		}
	}
	err := f.ValidateRepository(format.Repository{Type: "proxy", Config: map[string]any{"x": 1}})
	if !errors.As(err, &violation) || violation.Code != "invalid_format_config" {
		t.Fatalf("config: err = %v", err)
	}
}

func TestPktLineRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	if err := writePktLine(&buffer, "command=fetch"); err != nil {
		t.Fatal(err)
	}
	_ = writeDelim(&buffer)
	_ = writePkt(&buffer, []byte("want abc"))
	_ = writeFlush(&buffer)
	if got := buffer.String(); got != "0012command=fetch\n0001000cwant abc0000" {
		t.Fatalf("encoded %q", got)
	}

	kind, line, err := readPktLine(&buffer)
	if err != nil || kind != pktData || line != "command=fetch" {
		t.Fatalf("first = %v %q %v", kind, line, err)
	}
	if kind, _, _ := readPkt(&buffer); kind != pktDelim {
		t.Fatalf("second kind = %v", kind)
	}
	if _, data, _ := readPkt(&buffer); string(data) != "want abc" {
		t.Fatalf("third = %q", data)
	}
	if kind, _, _ := readPkt(&buffer); kind != pktFlush {
		t.Fatalf("fourth kind = %v", kind)
	}
	if _, _, err := readPkt(&buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("end err = %v", err)
	}
	if _, _, err := readPkt(strings.NewReader("00")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated err = %v", err)
	}
	if err := writePkt(io.Discard, make([]byte, maxPktData+1)); !errors.Is(err, errPktTooLong) {
		t.Fatalf("oversized err = %v", err)
	}
}

func TestSideBandSplitsLargeWrites(t *testing.T) {
	var buffer bytes.Buffer
	payload := bytes.Repeat([]byte{'x'}, maxSideBandData+10)
	if _, err := (sideBandWriter{w: &buffer}).Write(payload); err != nil {
		t.Fatal(err)
	}
	var reassembled []byte
	for count := 0; count < 2; count++ {
		kind, data, err := readPkt(&buffer)
		if err != nil || kind != pktData || data[0] != sideBandData {
			t.Fatalf("packet %d = %v %v", count, kind, err)
		}
		reassembled = append(reassembled, data[1:]...)
	}
	if !bytes.Equal(reassembled, payload) {
		t.Fatal("side-band payload does not round-trip")
	}
}

func TestParseCommandRequest(t *testing.T) {
	var body pktBuffer
	body.line("command=fetch").line("agent=git/2.55").line("object-format=sha1").delim().
		line("thin-pack").line("want " + strings.Repeat("a", 40)).line("done").flush()
	request, err := parseCommandRequest(&body)
	if err != nil {
		t.Fatal(err)
	}
	if request.command != "fetch" || len(request.capabilities) != 2 || len(request.args) != 3 {
		t.Fatalf("request = %+v", request)
	}

	var noArgs pktBuffer
	noArgs.line("command=ls-refs").flush()
	request, err = parseCommandRequest(&noArgs)
	if err != nil || request.command != "ls-refs" || len(request.args) != 0 {
		t.Fatalf("no-args request = %+v %v", request, err)
	}

	var bad pktBuffer
	bad.line("want abc").flush()
	if _, err := parseCommandRequest(&bad); err == nil {
		t.Fatal("request without command accepted")
	}
}

func TestParseRefsAndAdvertisement(t *testing.T) {
	oid := strings.Repeat("1", 40)
	tagged := strings.Repeat("2", 40)
	var body pktBuffer
	body.line(oid + " HEAD symref-target:refs/heads/main").
		line(oid + " refs/heads/main").
		line(tagged + " refs/tags/v1 peeled:" + oid).
		line("unborn refs/heads/wip symref-target:refs/heads/none").
		flush()
	refs, err := parseRefs(&body)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || refs[0].symrefTarget != "refs/heads/main" || refs[2].peeled != oid {
		t.Fatalf("refs = %+v", refs)
	}

	var advertisement pktBuffer
	advertisement.line("# service=git-upload-pack").flush().
		line("version 2").line("agent=git/2.55").line("ls-refs=unborn").
		line("fetch=shallow wait-for-done filter").line("object-format=sha1").flush()
	if err := parseAdvertisement(&advertisement); err != nil {
		t.Fatalf("v2 advertisement rejected: %v", err)
	}

	var noShallow pktBuffer
	noShallow.line("version 2").line("ls-refs").line("fetch").flush()
	if err := parseAdvertisement(&noShallow); err == nil {
		t.Fatal("advertisement without shallow accepted")
	}

	var v0 pktBuffer
	v0.line("# service=git-upload-pack").flush().line(oid + " HEAD\x00multi_ack side-band-64k").flush()
	if err := parseAdvertisement(&v0); err == nil {
		t.Fatal("v0 advertisement accepted")
	}
}

func buildPack(t *testing.T, objects uint32, corrupt bool) []byte {
	t.Helper()
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, objects)
	pack.WriteString("object bytes stand-in")
	sum := sha1.Sum(pack.Bytes())
	if corrupt {
		sum[0] ^= 0xff
	}
	pack.Write(sum[:])
	return pack.Bytes()
}

func TestPackVerifier(t *testing.T) {
	valid := buildPack(t, 1, false)
	got, err := io.ReadAll(newPackVerifier(bytes.NewReader(valid)))
	if err != nil || !bytes.Equal(got, valid) {
		t.Fatalf("valid pack: err = %v", err)
	}
	// Feed the pack byte by byte so the header and trailer straddle reads.
	got, err = io.ReadAll(newPackVerifier(iotest(valid)))
	if err != nil || !bytes.Equal(got, valid) {
		t.Fatalf("valid pack, small reads: err = %v", err)
	}
	if _, err := io.ReadAll(newPackVerifier(bytes.NewReader(buildPack(t, 1, true)))); err == nil {
		t.Fatal("corrupted checksum accepted")
	}
	if _, err := io.ReadAll(newPackVerifier(bytes.NewReader(buildPack(t, 0, false)))); err == nil {
		t.Fatal("empty pack accepted")
	}
	if _, err := io.ReadAll(newPackVerifier(strings.NewReader("ERR not a pack at all, honestly"))); err == nil {
		t.Fatal("non-pack accepted")
	}
	if _, err := io.ReadAll(newPackVerifier(bytes.NewReader(valid[:20]))); err == nil {
		t.Fatal("truncated pack accepted")
	}
}

// iotest returns a reader that yields one byte per Read.
func iotest(data []byte) io.Reader {
	return &oneByteReader{data: data}
}

type oneByteReader struct {
	data []byte
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestDemuxPackfile(t *testing.T) {
	pack := buildPack(t, 1, false)
	var response pktBuffer
	response.line("shallow-info").line("shallow " + strings.Repeat("a", 40)).delim().line("packfile")
	_ = writeSideBand(&response, sideBandProgress, []byte("Counting objects\r"))
	_ = writeSideBand(&response, sideBandData, pack[:10])
	_ = writeSideBand(&response, sideBandData, pack[10:])
	response.flush()

	var out bytes.Buffer
	if err := demuxPackfile(&response, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), pack) {
		t.Fatal("demuxed pack differs")
	}

	var failing pktBuffer
	failing.line("packfile")
	_ = writeSideBand(&failing, sideBandError, []byte("upload-pack: not our ref"))
	failing.flush()
	if err := demuxPackfile(&failing, io.Discard); err == nil || !strings.Contains(err.Error(), "not our ref") {
		t.Fatalf("error band err = %v", err)
	}

	var errLine pktBuffer
	errLine.line("ERR access denied").flush()
	if err := demuxPackfile(&errLine, io.Discard); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("ERR line err = %v", err)
	}
}

func TestFetchSnapshotBoundary(t *testing.T) {
	commit := strings.Repeat("a", 40)
	var shallow pktBuffer
	shallow.line("shallow-info").line("shallow " + commit).delim().line("packfile")
	if got, err := readPackfileSection(&shallow); err != nil || got != commit {
		t.Fatalf("upstream shallow boundary = %q, %v", got, err)
	}
	var complete pktBuffer
	complete.line("packfile")
	if got, err := readPackfileSection(&complete); err != nil || got != "" {
		t.Fatalf("complete upstream boundary = %q, %v", got, err)
	}
	for _, test := range []struct {
		contentType string
		boundary    string
		known       bool
		valid       bool
	}{
		{contentTypePackfile + "; shallow=" + commit, commit, true, true},
		{contentTypePackfile + "; snapshot=complete", "", true, true},
		{contentTypePackfile, "", false, true}, // legacy cached pack
		{contentTypePackfile + "; shallow=invalid", "", false, false},
	} {
		got, known, err := snapshotBoundary(test.contentType)
		if got != test.boundary || known != test.known || (err == nil) != test.valid {
			t.Errorf("snapshotBoundary(%q) = %q, %v, %v", test.contentType, got, known, err)
		}
	}
}
