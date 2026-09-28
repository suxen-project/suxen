package content

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/suxen-project/suxen/internal/domain"
)

type StagedUpload struct {
	Path   string
	Digest string
	Size   int64
	owner  *stagingFile
}

// ErrUploadSourceRead identifies a failure while reading the supplied upload
// body. Staging filesystem failures are deliberately left outside this class.
var ErrUploadSourceRead = errors.New("read upload body")

type uploadSourceReader struct{ io.Reader }

func (source uploadSourceReader) Read(p []byte) (int, error) {
	n, err := source.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("%w: %w", ErrUploadSourceRead, err)
	}
	return n, err
}

type stagingFile struct {
	file    *os.File
	once    sync.Once
	rt      *Runtime
	writing bool // protected by rt.stagingMu
}

func (upload StagedUpload) Remove() {
	if upload.owner != nil {
		upload.owner.once.Do(func() {
			_ = upload.owner.file.Close()
			upload.owner.rt.stagingMu.Lock()
			delete(upload.owner.rt.stagingFiles, upload.Path)
			upload.owner.rt.stagingMu.Unlock()
		})
	}
	_ = os.Remove(upload.Path)
}

func (upload StagedUpload) remove() {
	upload.Remove()
}

func (rt *Runtime) StageUpload(w http.ResponseWriter, source io.Reader) (StagedUpload, error) {
	return rt.stageUpload(w, source)
}

// stageUpload copies source to a temporary file while hashing it. w may be
// nil: MaxBytesReader then still enforces the size limit and only skips the
// connection-close hint it would otherwise set on the response.
func (rt *Runtime) stageUpload(w http.ResponseWriter, source io.Reader) (StagedUpload, error) {
	uploadDirectory := filepath.Join(rt.Config.DataDir, "uploads")
	if err := os.MkdirAll(uploadDirectory, 0o750); err != nil {
		return StagedUpload{}, fmt.Errorf("create upload directory: %w", err)
	}

	temporary, err := os.CreateTemp(uploadDirectory, "upload-*")
	if err != nil {
		return StagedUpload{}, fmt.Errorf("create temporary upload: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := lockStagingFile(temporary, false); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return StagedUpload{}, fmt.Errorf("lock temporary upload: %w", err)
	}
	owner := &stagingFile{file: temporary, rt: rt, writing: true}
	rt.stagingMu.Lock()
	if rt.stagingFiles == nil {
		rt.stagingFiles = make(map[string]*stagingFile)
	}
	rt.stagingFiles[temporaryPath] = owner
	rt.stagingMu.Unlock()

	hash := sha256.New()
	limited := http.MaxBytesReader(w, io.NopCloser(source), rt.Config.MaxUploadBytes)
	size, copyErr := io.Copy(io.MultiWriter(temporary, hash), uploadSourceReader{limited})
	if copyErr != nil {
		(StagedUpload{Path: temporaryPath, owner: owner}).Remove()
		return StagedUpload{}, fmt.Errorf("copy upload: %w", copyErr)
	}
	rt.stagingMu.Lock()
	owner.writing = false
	rt.stagingMu.Unlock()
	return StagedUpload{
		Path:   temporaryPath,
		Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		Size:   size,
		owner:  owner,
	}, nil
}

// CloseStaging releases ownership when a runtime stops. A crashed process has
// the same effect: the OS releases its advisory locks, leaving old files for GC.
func (rt *Runtime) CloseStaging() {
	rt.stagingMu.Lock()
	defer rt.stagingMu.Unlock()
	for path, owner := range rt.stagingFiles {
		if owner.writing {
			// In-flight slow requests remain protected even if shutdown
			// overlaps their body copy. Process exit releases the lock.
			continue
		}
		_ = owner.file.Close()
		delete(rt.stagingFiles, path)
	}
}

// ReapStaleStaging removes abandoned generic upload files. The minimum idle
// period keeps a just-created file safe during the short create-to-lock window.
// Once locked, even a slow upload remains protected across processes.
func (rt *Runtime) ReapStaleStaging(dryRun bool, now time.Time) (int, int, error) {
	directory := filepath.Join(rt.Config.DataDir, "uploads")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read staging directory: %w", err)
	}
	stale, deleted := 0, 0
	cutoff := now.Add(-time.Hour)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "upload-") {
			continue
		}
		name := filepath.Join(directory, entry.Name())
		file, err := os.OpenFile(name, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return stale, deleted, fmt.Errorf("open staged upload: %w", err)
		}
		locked, lockErr := tryLockStagingFile(file)
		if lockErr != nil {
			_ = file.Close()
			return stale, deleted, fmt.Errorf("lock staged upload: %w", lockErr)
		}
		if !locked {
			_ = file.Close()
			continue
		}
		info, err := file.Stat()
		pathInfo, pathErr := os.Lstat(name)
		if err == nil && pathErr == nil && os.SameFile(info, pathInfo) && info.Mode().IsRegular() && !info.ModTime().After(cutoff) {
			stale++
			if !dryRun {
				// No owner can attach to an existing staging file: its lock
				// is acquired at creation and held until removal or shutdown.
				// Close before unlink because Windows does not allow unlinking
				// a file opened without FILE_SHARE_DELETE.
				_ = file.Close()
				removeErr := os.Remove(name)
				if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return stale, deleted, fmt.Errorf("remove staged upload: %w", removeErr)
				}
				if removeErr == nil {
					deleted++
				}
				continue
			}
		}
		_ = file.Close()
	}
	return stale, deleted, nil
}

func openStagedUpload(upload StagedUpload) (*os.File, error) {
	file, err := os.Open(upload.Path)
	if err != nil {
		return nil, fmt.Errorf("open staged upload: %w", err)
	}
	return file, nil
}

func VerifyRequestedDigest(header, actual string) error {
	return verifyRequestedDigest(header, actual)
}

func IsUploadTooLarge(err error) bool {
	return isUploadTooLarge(err)
}

func ContentType(header, assetPath string) string {
	return contentType(header, assetPath)
}

func QuoteETag(digest string) string {
	return quoteETag(digest)
}

func verifyRequestedDigest(header, actual string) error {
	if header == "" {
		return nil
	}

	requested := strings.TrimSpace(strings.TrimPrefix(header, "sha-256="))
	requested = strings.Trim(requested, ":")
	if !strings.HasPrefix(requested, "sha256:") {
		return nil
	}
	if requested != actual {
		return fmt.Errorf(
			"%w: expected %s, got %s",
			domain.ErrDigestMismatch,
			requested,
			actual,
		)
	}
	return nil
}

func isUploadTooLarge(err error) bool {
	var maxBytesError *http.MaxBytesError
	return errors.As(err, &maxBytesError)
}

func contentType(header, assetPath string) string {
	if header != "" {
		return header
	}
	if detected := mime.TypeByExtension(path.Ext(assetPath)); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

func quoteETag(digest string) string {
	return `"` + digest + `"`
}
