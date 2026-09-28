package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/suxen-project/suxen/internal/domain"
)

// FS stores blobs in a sharded directory tree on a local or shared filesystem.
type FS struct {
	root string
}

// NewFS creates a filesystem blob store rooted at root.
func NewFS(root string) (*FS, error) {
	if root == "" {
		return nil, errors.New("blob root is empty")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &FS{root: root}, nil
}

// NormalizeDigest validates a SHA-256 digest and returns its canonical form.
func NormalizeDigest(digest string) (string, error) {
	algorithm, hash, ok := strings.Cut(strings.ToLower(digest), ":")
	if !ok {
		algorithm, hash = "sha256", strings.ToLower(digest)
	}
	if algorithm != "sha256" || len(hash) != 64 {
		return "", fmt.Errorf("invalid sha256 digest %q", digest)
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", fmt.Errorf("invalid digest: %w", err)
	}
	return "sha256:" + hash, nil
}

func (s *FS) path(d string) (string, error) {
	d, err := NormalizeDigest(d)
	if err != nil {
		return "", err
	}
	h := strings.TrimPrefix(d, "sha256:")
	return filepath.Join(s.root, "sha256", h[:2], h[2:4], h), nil
}

// Put persists a blob after verifying the supplied SHA-256 digest.
func (s *FS) Put(ctx context.Context, expected string, r io.Reader) (domain.BlobInfo, error) {
	expected, err := NormalizeDigest(expected)
	if err != nil {
		return domain.BlobInfo{}, err
	}
	dst, err := s.path(expected)
	if err != nil {
		return domain.BlobInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return domain.BlobInfo{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return domain.BlobInfo{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), &contextReader{ctx: ctx, r: r})
	if copyErr != nil {
		_ = tmp.Close()
		return domain.BlobInfo{}, copyErr
	}
	actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		_ = tmp.Close()
		return domain.BlobInfo{}, fmt.Errorf("%w: expected %s, got %s", domain.ErrDigestMismatch, expected, actual)
	}
	if existing, err := os.Stat(dst); err == nil {
		_ = tmp.Close()
		return blobInfo(expected, existing), nil
	}
	// Sync the writable staging handle before closing it. Reopening the file
	// read-only and calling Sync fails with access denied on Windows.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return domain.BlobInfo{}, err
	}
	if err := tmp.Close(); err != nil {
		return domain.BlobInfo{}, err
	}
	if err := replaceFile(tmpName, dst); err != nil {
		if st, statErr := os.Stat(dst); statErr == nil {
			return blobInfo(expected, st), nil
		}
		return domain.BlobInfo{}, err
	}
	// Persist the rename itself. This is required for an acknowledged upload to
	// survive an abrupt restart. Unix fsyncs the parent directory; Windows uses
	// MoveFileEx with MOVEFILE_WRITE_THROUGH in replaceFile.
	if err := syncParentDirectory(filepath.Dir(dst)); err != nil {
		return domain.BlobInfo{}, err
	}
	persisted, err := os.Stat(dst)
	if err != nil {
		return domain.BlobInfo{}, fmt.Errorf("stat persisted blob: %w", err)
	}
	return blobInfo(expected, persisted), nil
}

// Get opens a blob and returns its metadata.
func (s *FS) Get(_ context.Context, digest string) (io.ReadCloser, domain.BlobInfo, error) {
	p, err := s.path(digest)
	if err != nil {
		return nil, domain.BlobInfo{}, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, domain.BlobInfo{}, domain.ErrNotFound
	}
	if err != nil {
		return nil, domain.BlobInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, domain.BlobInfo{}, err
	}
	d, _ := NormalizeDigest(digest)
	return f, blobInfo(d, st), nil
}

// Head returns blob metadata without retaining an open file.
func (s *FS) Head(ctx context.Context, d string) (domain.BlobInfo, error) {
	reader, info, err := s.Get(ctx, d)
	if reader != nil {
		_ = reader.Close()
	}
	return info, err
}

// Walk yields every valid SHA-256 blob in the sharded store.
func (s *FS) Walk(ctx context.Context, yield func(domain.BlobInfo) error) error {
	root := filepath.Join(s.root, "sha256")
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || len(entry.Name()) != 64 {
			return nil
		}
		digest, err := NormalizeDigest(entry.Name())
		if err != nil {
			return nil
		}
		canonicalPath, err := s.path(digest)
		if err != nil || filePath != canonicalPath {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			// Another operation may remove a blob after its directory was read.
			// Skip that entry without terminating the rest of the walk.
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return yield(blobInfo(digest, info))
	})
	if err != nil {
		// Callback errors must propagate, including missing-file errors from
		// a migration target. Only enumeration errors are handled above.
		return err
	}
	return ctx.Err()
}

// Delete removes a blob if it exists.
func (s *FS) Delete(_ context.Context, d string) error {
	p, e := s.path(d)
	if e != nil {
		return e
	}
	e = os.Remove(p)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	return e
}

// CreateUpload creates an empty transient upload in the filesystem store.
func (s *FS) CreateUpload(_ context.Context, key string) error {
	uploadPath, err := s.uploadPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(uploadPath), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(uploadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrUploadExists, key)
	}
	if err != nil {
		return err
	}
	return file.Close()
}

// AppendUpload atomically appends source to a transient filesystem upload.
func (s *FS) AppendUpload(
	ctx context.Context,
	key string,
	source io.Reader,
	maxSize int64,
) (int64, error) {
	uploadPath, err := s.uploadPath(key)
	if err != nil {
		return 0, err
	}
	existing, err := os.Open(uploadPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, domain.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	info, err := existing.Stat()
	if err != nil {
		_ = existing.Close()
		return 0, err
	}
	initialSize := info.Size()
	if initialSize > maxSize {
		_ = existing.Close()
		return initialSize, ErrUploadTooLarge
	}

	staged, err := os.CreateTemp(filepath.Dir(uploadPath), ".append-*")
	if err != nil {
		_ = existing.Close()
		return initialSize, err
	}
	stagedPath := staged.Name()
	defer os.Remove(stagedPath)

	copiedExisting, copyErr := io.Copy(
		staged,
		&contextReader{ctx: ctx, r: io.LimitReader(existing, limitPlusOne(initialSize))},
	)
	if copyErr == nil && copiedExisting != initialSize {
		copyErr = fmt.Errorf("upload size changed while reading")
	}
	if closeErr := existing.Close(); copyErr == nil && closeErr != nil {
		copyErr = closeErr
	}
	remaining := maxSize - initialSize
	written := int64(0)
	if copyErr == nil {
		written, copyErr = io.Copy(
			staged,
			io.LimitReader(&contextReader{ctx: ctx, r: source}, limitPlusOne(remaining)),
		)
	}
	if copyErr == nil && written > remaining {
		copyErr = ErrUploadTooLarge
	}
	if copyErr != nil {
		_ = staged.Close()
		return initialSize, copyErr
	}
	if err := staged.Chmod(0o640); err != nil {
		_ = staged.Close()
		return initialSize, err
	}
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return initialSize, err
	}
	if err := staged.Close(); err != nil {
		return initialSize, err
	}
	if err := replaceFile(stagedPath, uploadPath); err != nil {
		return initialSize, err
	}
	if err := syncParentDirectory(filepath.Dir(uploadPath)); err != nil {
		return initialSize, err
	}
	return initialSize + written, nil
}

// OpenUpload opens a transient filesystem upload for streaming.
func (s *FS) OpenUpload(
	_ context.Context,
	key string,
) (io.ReadCloser, int64, error) {
	uploadPath, err := s.uploadPath(key)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(uploadPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, domain.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

// DeleteUpload removes a transient filesystem upload.
func (s *FS) DeleteUpload(_ context.Context, key string) error {
	uploadPath, err := s.uploadPath(key)
	if err != nil {
		return err
	}
	err = os.Remove(uploadPath)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ErrNotFound
	}
	return err
}

func (s *FS) uploadPath(key string) (string, error) {
	cleaned, err := cleanUploadKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, ".suxen-uploads", filepath.FromSlash(cleaned)), nil
}

// Ready verifies that the blob root can create and remove a file.
func (s *FS) Ready(_ context.Context) error {
	file, err := os.CreateTemp(s.root, ".ready-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func blobInfo(digest string, info os.FileInfo) domain.BlobInfo {
	return domain.BlobInfo{
		Digest:     digest,
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

var _ Store = (*FS)(nil)
var _ UploadStore = (*FS)(nil)
