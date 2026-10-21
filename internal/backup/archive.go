// Package backup implements per-instance tar.gz archives, safe extraction and
// the automatic backup scheduler. All writes are idempotent and every failure
// is reported before a partial archive is left behind.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// logRetention is how old files under logs/ must be to be excluded from an
	// archive (spec M6: exclude logs older than 7 days).
	logRetention = 7 * 24 * time.Hour
	// maxArchiveEntries and maxArchiveBytes bound extraction so a malicious
	// upload cannot exhaust the host.
	maxArchiveEntries = 200000
	maxArchiveBytes   = int64(8 << 30)
	maxArchiveFile    = int64(4 << 30)

	backupDirMode  = 0o700
	backupFileMode = 0o600
)

// archiveFileRE pins the generated names: <instance-id>-<YYYYmmdd>-<HHMMSS>[-n].tar.gz
var archiveFileRE = regexp.MustCompile(`^[a-z0-9-]{1,20}-\d{8}-\d{6}(?:-\d{1,3})?\.tar\.gz$`)

var (
	ErrInstanceDirMissing = errors.New("instance data directory is missing")
	ErrArchiveInvalid     = errors.New("backup archive is invalid")
	ErrArchiveMissing     = errors.New("backup archive does not exist")
	ErrBackupNotFound     = errors.New("backup record not found")
	ErrArchiveUnsafe      = errors.New("backup archive contains an unsafe path or entry")
	ErrArchiveNoDatabase  = errors.New("backup archive does not contain ts3server.sqlitedb")
)

// DiskSpaceError reports the exact shortfall so the API can explain it.
type DiskSpaceError struct {
	Free    int64
	Needed  int64
	DirSize int64
}

func (e *DiskSpaceError) Error() string {
	return fmt.Sprintf("not enough free disk space: need %d bytes (2x %d), have %d", e.Needed, e.DirSize, e.Free)
}

func isValidArchiveName(name string) bool {
	return archiveFileRE.MatchString(name)
}

// archiveName builds a unique file name. Two backups within the same second
// would otherwise collide; the suffix keeps both.
func archiveName(id string, at time.Time, exists func(string) bool) string {
	base := fmt.Sprintf("%s-%s", id, at.UTC().Format("20060102-150405"))
	name := base + ".tar.gz"
	if exists == nil || !exists(name) {
		return name
	}
	for n := 2; n < 1000; n++ {
		candidate := fmt.Sprintf("%s-%d.tar.gz", base, n)
		if !exists(candidate) {
			return candidate
		}
	}
	return base + "-999.tar.gz"
}

// dirSizeBytes sums regular file sizes under root. Unreadable entries are
// skipped: the check is advisory and must not fail because of one file owner.
func dirSizeBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// createArchive writes a gzip-compressed tar of srcDir to dst. Files under
// logs/ whose mtime is older than seven days are skipped. The archive contains
// paths relative to srcDir, so extraction into the instance directory restores
// the exact layout.
func createArchive(ctx context.Context, srcDir, dst string, now time.Time) (int64, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, backupFileMode)
	if err != nil {
		return 0, fmt.Errorf("create archive: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = out.Close()
			_ = os.Remove(dst)
		}
	}()

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	cutoff := now.Add(-logRetention)

	walkErr := filepath.WalkDir(srcDir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Exclude stale logs: they are the bulk of the directory and carry no
		// identity value. Recent logs are kept for post-mortem analysis.
		if strings.HasPrefix(rel, "logs/") && !info.IsDir() && info.ModTime().Before(cutoff) {
			return nil
		}
		// Symlinks and special files are not part of a TS3 data directory and
		// are rejected on restore anyway, so they are skipped on creation too.
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if info.IsDir() {
			header.Name += "/"
		}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		file, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(tw, file, info.Size())
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return 0, fmt.Errorf("archive %s: %w", srcDir, walkErr)
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return 0, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("close gzip: %w", err)
	}
	if err := out.Sync(); err != nil {
		return 0, fmt.Errorf("fsync archive: %w", err)
	}
	if err := out.Close(); err != nil {
		return 0, fmt.Errorf("close archive: %w", err)
	}
	committed = true
	fi, err := os.Stat(dst)
	if err != nil {
		return 0, fmt.Errorf("stat archive: %w", err)
	}
	return fi.Size(), nil
}

// archiveEntry is one validated header. Only the fields needed by extraction
// are kept so validation does not hold the whole archive in memory.
type archiveEntry struct {
	Name     string
	Typeflag byte
	Mode     fs.FileMode
	Size     int64
	ModTime  time.Time
}

// inspectArchive validates the whole archive without extracting it: no
// absolute paths, no "..", no symlinks/hardlinks/device nodes and a root-level
// ts3server.sqlitedb. It is cheap enough to run before stopping the container.
func inspectArchive(src string) ([]archiveEntry, error) {
	f, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrArchiveMissing
		}
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzip file", ErrArchiveInvalid)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var (
		entries []archiveEntry
		total   int64
		hasDB   bool
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrArchiveInvalid, err)
		}
		if len(entries) >= maxArchiveEntries {
			return nil, fmt.Errorf("%w: too many entries", ErrArchiveInvalid)
		}
		name, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrArchiveUnsafe, hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > maxArchiveFile {
				return nil, fmt.Errorf("%w: file %q is too large", ErrArchiveInvalid, name)
			}
			total += hdr.Size
			if total > maxArchiveBytes {
				return nil, fmt.Errorf("%w: uncompressed size exceeds limit", ErrArchiveInvalid)
			}
			if name == "ts3server.sqlitedb" {
				hasDB = true
			}
		case tar.TypeDir:
		default:
			return nil, fmt.Errorf("%w: unsupported entry type for %q", ErrArchiveUnsafe, hdr.Name)
		}
		entries = append(entries, archiveEntry{
			Name:     name,
			Typeflag: hdr.Typeflag,
			Mode:     fs.FileMode(hdr.Mode).Perm(),
			Size:     hdr.Size,
			ModTime:  hdr.ModTime,
		})
	}
	if !hasDB {
		return nil, ErrArchiveNoDatabase
	}
	return entries, nil
}

// extractArchive writes the archive into dst, which must be a fresh directory.
// Paths are re-validated here as well so a TOCTOU change between validation and
// extraction cannot escape dst.
func extractArchive(ctx context.Context, src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrArchiveMissing
		}
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%w: not a gzip file", ErrArchiveInvalid)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrArchiveInvalid, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		name, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return fmt.Errorf("%w: %q", ErrArchiveUnsafe, hdr.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if !withinDir(dst, target) {
			return fmt.Errorf("%w: %q escapes destination", ErrArchiveUnsafe, hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create directory %q: %w", name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create parent of %q: %w", name, err)
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return fmt.Errorf("create file %q: %w", name, err)
			}
			_, copyErr := io.CopyN(file, tr, hdr.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %q: %w", name, closeErr)
			}
			if !hdr.ModTime.IsZero() {
				_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			}
		default:
			return fmt.Errorf("%w: unsupported entry type for %q", ErrArchiveUnsafe, hdr.Name)
		}
	}
	return nil
}

// cleanArchivePath normalises a tar entry name and rejects anything that could
// escape the extraction directory. Backslashes are rejected outright because
// they are separators on Windows and would hide traversal there.
func cleanArchivePath(name string) (string, error) {
	if strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		return "", ErrArchiveUnsafe
	}
	if strings.HasPrefix(name, "/") || path.IsAbs(name) {
		return "", ErrArchiveUnsafe
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", ErrArchiveUnsafe
	}
	return clean, nil
}

func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
