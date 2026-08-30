package local

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

func validateRelativeID(id string) error {
	if id == "" || len(id) > 1024 || !utf8.ValidString(id) || id != strings.TrimSpace(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	if path.IsAbs(id) || filepath.IsAbs(id) || filepath.VolumeName(id) != "" ||
		strings.ContainsAny(id, "\\:\x00") || path.Clean(id) != id {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	segments := strings.Split(id, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || len(segment) > 255 ||
			segment != strings.TrimSpace(segment) {
			return fmt.Errorf("%w: %q", ErrInvalidID, id)
		}
		for _, character := range segment {
			if !isIDCharacter(character) {
				return fmt.Errorf("%w: %q", ErrInvalidID, id)
			}
		}
	}
	return nil
}

func isIDCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		character == '.' || character == '_' || character == '~' || character == '-'
}

func validateLabel(name, value string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) {
		return fmt.Errorf("%w: %s is invalid", ErrInvalidID, name)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%w: %s contains control characters", ErrInvalidID, name)
		}
	}
	return nil
}

func (store *Store) artifactPath(digest string, createParent bool) (string, error) {
	if !isLowerSHA256(digest) {
		return "", fmt.Errorf("%w: invalid artifact digest", ErrInvalidID)
	}
	directory, err := store.ensureDirectory("artifacts/sha256/"+digest[:2], createParent)
	if err != nil {
		return "", err
	}
	return store.containedPath(filepath.Join(directory, digest))
}

func (store *Store) streamPath(stream string, createParent bool) (string, error) {
	return store.idFilePath("streams", stream, ".jsonl", createParent)
}

func (store *Store) immutablePath(id string, createParent bool) (string, error) {
	return store.idFilePath("immutable", id, ".json", createParent)
}

func (store *Store) idFilePath(namespace, id, suffix string, createParent bool) (string, error) {
	if err := validateRelativeID(id); err != nil {
		return "", err
	}
	relative := filepath.FromSlash(id)
	directoryID := filepath.Dir(relative)
	relativeDirectory := namespace
	if directoryID != "." {
		relativeDirectory = filepath.Join(namespace, directoryID)
	}
	directory, err := store.ensureDirectory(filepath.ToSlash(relativeDirectory), createParent)
	if err != nil {
		return "", err
	}
	return store.containedPath(filepath.Join(directory, filepath.Base(relative)+suffix))
}

func (store *Store) lockPath(namespace, id string) (string, error) {
	return store.idFilePath(filepath.ToSlash(filepath.Join(".locks", namespace)), id, ".lock", true)
}

func (store *Store) fixedLockPath(relative string) (string, error) {
	clean := path.Clean(relative)
	if clean != relative || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: invalid fixed lock path", ErrUnsafePath)
	}
	directory, err := store.ensureDirectory(filepath.ToSlash(filepath.Join(".locks", filepath.Dir(relative))), true)
	if err != nil {
		return "", err
	}
	return store.containedPath(filepath.Join(directory, filepath.Base(relative)))
}

func (store *Store) ensureDirectory(relative string, create bool) (string, error) {
	if relative == "" || path.IsAbs(relative) || path.Clean(relative) != relative ||
		strings.HasPrefix(relative, "../") || strings.Contains(relative, "\\") {
		return "", fmt.Errorf("%w: invalid internal directory %q", ErrUnsafePath, relative)
	}
	current := store.root
	for _, segment := range strings.Split(relative, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("%w: invalid internal directory %q", ErrUnsafePath, relative)
		}
		next := filepath.Join(current, segment)
		info, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return "", &os.PathError{Op: "open", Path: next, Err: os.ErrNotExist}
			}
			if err := os.Mkdir(next, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("create store directory %q: %w", relative, err)
			}
			info, err = os.Lstat(next)
			if err == nil {
				if syncErr := syncDirectory(current); syncErr != nil {
					return "", fmt.Errorf("sync store directory parent: %w", syncErr)
				}
			}
		}
		if err != nil {
			return "", fmt.Errorf("inspect store directory %q: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: %q is not a real directory", ErrUnsafePath, next)
		}
		current = next
	}
	return store.containedPath(current)
}

func (store *Store) containedPath(target string) (string, error) {
	clean := filepath.Clean(target)
	relative, err := filepath.Rel(store.root, clean)
	if err != nil {
		return "", fmt.Errorf("check store containment: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: %q escapes store root", ErrUnsafePath, target)
	}
	return clean, nil
}

func openRegularNoFollow(target string) (*os.File, error) {
	info, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q is not a regular file", ErrUnsafePath, target)
	}
	file, err := os.OpenFile(target, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open regular file: %w", err)
	}
	openedInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat opened file: %w", err)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%w: file changed while opening %q", ErrUnsafePath, target)
	}
	return file, nil
}

func openAppendNoFollow(target string) (*os.File, error) {
	file, err := os.OpenFile(
		target,
		os.O_CREATE|os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open append stream: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat append stream: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%w: append target is not regular", ErrUnsafePath)
	}
	lstat, err := os.Lstat(target)
	if err != nil || lstat.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, lstat) {
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect append stream: %w", err)
		}
		return nil, fmt.Errorf("%w: append target changed while opening", ErrUnsafePath)
	}
	return file, nil
}

func acquireFileLock(target string, shared bool) (*os.File, error) {
	file, err := os.OpenFile(
		target,
		os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open store lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("stat store lock: %w", err)
		}
		return nil, fmt.Errorf("%w: lock target is not regular", ErrUnsafePath)
	}
	lstat, err := os.Lstat(target)
	if err != nil || lstat.Mode()&os.ModeSymlink != 0 ||
		!lstat.Mode().IsRegular() || !os.SameFile(info, lstat) {
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect store lock: %w", err)
		}
		return nil, fmt.Errorf("%w: lock target changed while opening", ErrUnsafePath)
	}
	operation := syscall.LOCK_EX
	if shared {
		operation = syscall.LOCK_SH
	}
	for {
		err = syscall.Flock(int(file.Fd()), operation)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("lock store file: %w", err)
	}
	lockedInfo, err := os.Lstat(target)
	if err != nil || lockedInfo.Mode()&os.ModeSymlink != 0 ||
		!lockedInfo.Mode().IsRegular() || !os.SameFile(info, lockedInfo) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("reinspect locked store file: %w", err)
		}
		return nil, fmt.Errorf("%w: lock target changed while acquiring", ErrUnsafePath)
	}
	return file, nil
}

func releaseFileLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func (store *Store) atomicCreate(target string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(target)
	if _, err := store.containedPath(target); err != nil {
		return err
	}
	if _, err := store.ensureDirectoryFromAbsolute(directory); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return ErrImmutableExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect atomic target: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".argus-tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := writeAll(temporary, data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("set immutable file mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if _, err := os.Lstat(target); err == nil {
		return ErrImmutableExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reinspect atomic target: %w", err)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	committed = true
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync atomic target directory: %w", err)
	}
	return nil
}

func (store *Store) ensureDirectoryFromAbsolute(directory string) (string, error) {
	relative, err := filepath.Rel(store.root, directory)
	if err != nil {
		return "", fmt.Errorf("resolve target directory: %w", err)
	}
	if relative == "." {
		return store.root, nil
	}
	return store.ensureDirectory(filepath.ToSlash(relative), false)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Sync(); err != nil &&
		!errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
