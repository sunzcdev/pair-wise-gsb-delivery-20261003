package sftp

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

var errLocalOperationUnsupported = errors.New("unsupported local operation")

// LocalFileHandler is a request-server backend which confines access to root.
//
// Paths passed by the request server are virtual, POSIX-style paths rooted at
// "/". The backend keeps root open as a directory handle and resolves each
// path relative to that handle, so replacing a directory after path cleanup
// cannot redirect an operation to another location.
//
// Symbolic links are followed, but a link is usable only when its target
// remains within root. Creating a symbolic link with an absolute target is
// treated as a virtual root path. Relative links are stored as written after
// being checked against the directory containing the link.
type LocalFileHandler struct {
	root *os.Root
}

// NewLocalFileHandler opens root and returns a request-server handler.
func NewLocalFileHandler(root string) (*LocalFileHandler, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &LocalFileHandler{root: r}, nil
}

// Close releases the directory handle backing the backend.
func (h *LocalFileHandler) Close() error {
	return h.root.Close()
}

// Handlers returns the four handler groups expected by NewRequestServer.
func (h *LocalFileHandler) Handlers() Handlers {
	return Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h}
}

// Fileread opens a file relative to the confined root for reading.
func (h *LocalFileHandler) Fileread(r *Request) (io.ReaderAt, error) {
	flags := r.Pflags()
	if !flags.Read || flags.Write || flags.Append || flags.Creat || flags.Trunc {
		return nil, os.ErrInvalid
	}
	return h.root.OpenFile(virtualToRootPath(r.Filepath), os.O_RDONLY, 0)
}

// Filewrite opens a file relative to the confined root for writing.
func (h *LocalFileHandler) Filewrite(r *Request) (io.WriterAt, error) {
	return h.OpenFile(r)
}

// OpenFile opens a file relative to the confined root for reading and writing.
func (h *LocalFileHandler) OpenFile(r *Request) (WriterAtReaderAt, error) {
	flags, err := localOpenFlags(r.Flags)
	if err != nil {
		return nil, err
	}
	return h.root.OpenFile(virtualToRootPath(r.Filepath), flags, 0o666)
}

// Filecmd applies a command relative to the confined root.
func (h *LocalFileHandler) Filecmd(r *Request) error {
	switch r.Method {
	case "Setstat":
		return h.setstat(r)
	case "Rename":
		return h.rename(virtualToRootPath(r.Filepath), virtualToRootPath(r.Target), true)
	case "Rmdir":
		return h.rmdir(virtualToRootPath(r.Filepath))
	case "Remove":
		return h.remove(virtualToRootPath(r.Filepath))
	case "Mkdir":
		return h.root.Mkdir(virtualToRootPath(r.Filepath), 0o777)
	case "Link":
		return h.link(virtualToRootPath(r.Filepath), virtualToRootPath(r.Target))
	case "Symlink":
		target, err := h.symlinkTarget(r.Filepath, r.Target)
		if err != nil {
			return err
		}
		return h.root.Symlink(target, virtualToRootPath(r.Target))
	}
	return errLocalOperationUnsupported
}

// PosixRename renames without requiring the target to be absent.
func (h *LocalFileHandler) PosixRename(r *Request) error {
	return h.rename(virtualToRootPath(r.Filepath), virtualToRootPath(r.Target), false)
}

// Filelist lists a directory, stats a path, or reads a symbolic link.
func (h *LocalFileHandler) Filelist(r *Request) (ListerAt, error) {
	name := virtualToRootPath(r.Filepath)
	switch r.Method {
	case "List":
		f, err := h.root.Open(name)
		if err != nil {
			return nil, err
		}
		infos, err := f.Readdir(-1)
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return listerat(infos), nil
	case "Stat":
		info, err := h.root.Stat(name)
		if err != nil {
			return nil, err
		}
		return listerat{info}, nil
	}
	return nil, errLocalOperationUnsupported
}

// Lstat stats a path without following the final symbolic link.
func (h *LocalFileHandler) Lstat(r *Request) (ListerAt, error) {
	info, err := h.root.Lstat(virtualToRootPath(r.Filepath))
	if err != nil {
		return nil, err
	}
	return listerat{info}, nil
}

// Readlink returns the stored target of a symbolic link in the root.
func (h *LocalFileHandler) Readlink(name string) (string, error) {
	target, err := h.root.Readlink(virtualToRootPath(name))
	if err != nil {
		return "", err
	}
	if _, err := resolvedLinkTarget(name, target); err != nil {
		return "", os.ErrPermission
	}
	return target, nil
}

// RealPath returns the normalized virtual path without resolving links.
func (h *LocalFileHandler) RealPath(name string) (string, error) {
	return cleanPathWithBase("/", name), nil
}

func (h *LocalFileHandler) setstat(r *Request) error {
	name := virtualToRootPath(r.Filepath)
	attrs := r.Attributes()
	flags := r.AttrFlags()

	var file *os.File
	var err error
	if flags.Size {
		file, err = h.root.OpenFile(name, os.O_WRONLY, 0)
	} else if flags.UidGid || flags.Acmodtime || flags.Permissions {
		file, err = h.root.Open(name)
	} else {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	if flags.Size {
		if err := file.Truncate(int64(attrs.Size)); err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := file.Chmod(attrs.FileMode()); err != nil {
			return err
		}
	}
	if flags.UidGid {
		if err := file.Chown(int(attrs.UID), int(attrs.GID)); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := h.chtimes(name, file, attrs.AccessTime(), attrs.ModTime()); err != nil {
			return err
		}
	}
	return nil
}

func localOpenFlags(requestFlags uint32) (int, error) {
	pflags := newFileOpenFlags(requestFlags)
	if !pflags.Write && !pflags.Append {
		return 0, os.ErrInvalid
	}

	flags := 0
	if pflags.Read {
		flags |= os.O_RDWR
	} else {
		flags |= os.O_WRONLY
	}
	if pflags.Creat {
		flags |= os.O_CREATE
	}
	if pflags.Trunc {
		flags |= os.O_TRUNC
	}
	if pflags.Excl {
		flags |= os.O_EXCL
	}
	return flags, nil
}

func virtualToRootPath(name string) string {
	components, err := virtualComponentsBelowRoot(name)
	if err != nil {
		return name
	}
	name = strings.Join(components, "/")
	if name == "" {
		return "."
	}
	if runtime.GOOS == "windows" {
		return filepath.FromSlash(name)
	}
	return name
}

func (h *LocalFileHandler) symlinkTarget(target, linkPath string) (string, error) {
	if target == "" {
		return "", os.ErrInvalid
	}
	if strings.HasPrefix(target, "/") {
		targetComponents, err := virtualComponentsBelowRoot(target)
		if err != nil {
			return "", err
		}
		target = "/" + strings.Join(targetComponents, "/")
	}

	targetComponents, err := resolvedLinkTarget(linkPath, target)
	if err != nil {
		return "", err
	}

	linkParent := path.Dir(cleanVirtualPath(linkPath))
	parentComponents, err := virtualComponentsBelowRoot(linkParent)
	if err != nil {
		return "", err
	}

	common := 0
	for common < len(parentComponents) && common < len(targetComponents) && parentComponents[common] == targetComponents[common] {
		common++
	}

	parts := make([]string, 0, len(parentComponents)-common+len(targetComponents)-common)
	for range parentComponents[common:] {
		parts = append(parts, "..")
	}
	parts = append(parts, targetComponents[common:]...)
	if len(parts) == 0 {
		return ".", nil
	}
	return filepath.FromSlash(strings.Join(parts, "/")), nil
}

func resolvedLinkTarget(linkVirtualPath, target string) ([]string, error) {
	if target == "" {
		return nil, os.ErrInvalid
	}
	if strings.HasPrefix(target, "/") {
		return virtualComponentsBelowRoot(target)
	}
	if isWindowsDriveRelative(target) {
		return nil, os.ErrPermission
	}

	linkParent := path.Dir(cleanVirtualPath(linkVirtualPath))
	stack, err := virtualComponentsBelowRoot(linkParent)
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(target, "/") {
		switch component {
		case "", ".":
		case "..":
			if len(stack) == 0 {
				return nil, os.ErrPermission
			}
			stack = stack[:len(stack)-1]
		default:
			if isWindowsDriveComponent(component) {
				return nil, os.ErrPermission
			}
			stack = append(stack, component)
		}
	}
	return stack, nil
}

func isWindowsDriveRelative(target string) bool {
	return runtime.GOOS == "windows" && len(target) >= 2 && target[1] == ':'
}

func virtualComponentsBelowRoot(name string) ([]string, error) {
	components, err := virtualComponents(name)
	if err != nil {
		return nil, err
	}
	depth := 0
	for _, component := range components {
		if component == ".." {
			depth--
			if depth < 0 {
				return nil, os.ErrPermission
			}
			continue
		}
		depth++
	}
	return components, nil
}

func virtualComponents(name string) ([]string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || !strings.HasPrefix(name, "/") {
		return nil, os.ErrInvalid
	}

	components := make([]string, 0, strings.Count(name, "/"))
	for _, component := range strings.Split(name, "/") {
		switch component {
		case "", ".":
		default:
			if isWindowsDriveComponent(component) {
				return nil, os.ErrPermission
			}
			components = append(components, component)
		}
	}
	return components, nil
}

func cleanVirtualPath(name string) string {
	components, err := virtualComponentsBelowRoot(name)
	if err != nil {
		return name
	}
	return "/" + strings.Join(components, "/")
}

func isWindowsDriveComponent(component string) bool {
	return runtime.GOOS == "windows" && len(component) == 2 && component[1] == ':'
}
