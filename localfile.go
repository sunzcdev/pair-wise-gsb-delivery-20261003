package sftp

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

// LocalFileHandler returns a Handlers backed by files on the local filesystem,
// confined to rootDir.
//
// Every request path is resolved relative to rootDir, and no symlink or ".."
// component is allowed to resolve outside of it. Resolution is performed by
// the operating system as part of the operation itself (using openat2 with
// RESOLVE_BENEATH on Linux, and the equivalent services on other supported
// platforms), so an attacker that replaces directories or symlinks between
// the request arriving and the file being touched cannot escape the root:
// there is no separate "clean the path and check a string prefix" step.
//
// Relative symlinks that stay inside rootDir (for example "a -> b/c" or
// "a -> ../b") keep working. A symlink whose target resolves outside rootDir
// cannot be created with an absolute target, and any attempt to access it
// fails instead of reaching the outside file.
//
// Both endpoints of rename and hard-link operations are resolved inside
// rootDir.
//
// The returned Handlers (and every method on them) is safe for concurrent use.
// The Close method closes the underlying root directory handle; it must not be
// called while the RequestServer is still serving.
//
// On plan9 and js/wasm the confinement guarantees of the underlying [os.Root]
// are weaker: plan9 resolves paths by name rather than file descriptor, and
// js does not close the symlink race window. See the [os.Root] documentation
// for the precise per-platform limitations.
func LocalFileHandler(rootDir string) (Handlers, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return Handlers{}, err
	}

	lf := &LocalFile{root: root}

	return Handlers{
		FileGet:  lf,
		FilePut:  lf,
		FileCmd:  lf,
		FileList: lf,
	}, nil
}

// LocalFile is a local filesystem backend for a RequestServer, rooted at the
// directory passed to [LocalFileHandler].
type LocalFile struct {
	root *os.Root
}

// Close releases the operating system resources held by the root directory.
func (lf *LocalFile) Close() error {
	return lf.root.Close()
}

// Name returns the path of the root directory, as presented to
// [LocalFileHandler].
func (lf *LocalFile) Name() string {
	return lf.root.Name()
}

// rel converts a Request path (an absolute POSIX style path, with "/"
// representing the root directory) into a name relative to the root, in the
// local operating system's separator convention.
func rel(p string) (string, error) {
	if !path.IsAbs(p) {
		// Request.Filepath and Request.Target are always cleaned absolute
		// POSIX paths, so this is defensive rather than expected.
		return "", &fs.PathError{Op: "openat", Path: p, Err: syscall.EINVAL}
	}

	if p == "/" {
		return ".", nil
	}

	// The request paths are cleaned with POSIX semantics. On Windows,
	// convert the separators. [os.Root] performs its own lexical and
	// kernel-enforced validation, so ".." components and symlinks that
	// escape the root are rejected during resolution, never by a string
	// prefix comparison.
	return filepath.FromSlash(p[1:]), nil
}

func (lf *LocalFile) Fileread(r *Request) (io.ReaderAt, error) {
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}

	return lf.root.Open(name)
}

func (lf *LocalFile) OpenFile(r *Request) (WriterAtReaderAt, error) {
	return lf.openFile(r, r.Pflags())
}

func (lf *LocalFile) Filewrite(r *Request) (io.WriterAt, error) {
	return lf.openFile(r, r.Pflags())
}

func (lf *LocalFile) openFile(r *Request, flags FileOpenFlags) (*os.File, error) {
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}

	flag := 0
	switch {
	case flags.Read && flags.Write:
		flag = os.O_RDWR
	case flags.Write || flags.Append:
		flag = os.O_WRONLY
	default:
		flag = os.O_RDONLY
	}

	// O_APPEND is intentionally never passed: [FileWriter] returns an
	// io.WriterAt, whose offsets are explicit and which is incompatible
	// with O_APPEND.
	if flags.Creat {
		flag |= os.O_CREATE
	}
	if flags.Trunc {
		flag |= os.O_TRUNC
	}
	if flags.Excl {
		flag |= os.O_EXCL
	}

	perm := os.FileMode(0o644)
	if r.AttrFlags().Permissions {
		perm = r.Attributes().FileMode().Perm()
	}

	return lf.root.OpenFile(name, flag, perm)
}

func (lf *LocalFile) Filecmd(r *Request) error {
	switch r.Method {
	case "Setstat":
		return lf.setstat(r)

	case "Rename":
		return lf.rename(r, false)

	case "PosixRename":
		return lf.rename(r, true)

	case "Rmdir":
		return lf.remove(r, true)

	case "Remove":
		return lf.remove(r, false)

	case "Mkdir":
		return lf.mkdir(r)

	case "Link":
		return lf.link(r)

	case "Symlink":
		return lf.symlink(r)
	}

	return errors.New("unsupported")
}

func (lf *LocalFile) PosixRename(r *Request) error {
	return lf.rename(r, true)
}

func (lf *LocalFile) setstat(r *Request) error {
	name, err := rel(r.Filepath)
	if err != nil {
		return err
	}

	attrFlags := r.AttrFlags()
	if !attrFlags.Size {
		// Metadata-only changes. [os.Root] resolves the name inside the
		// root for each operation; symlinks cannot point outside.
		attrs := r.Attributes()

		if attrFlags.Permissions {
			if err := lf.root.Chmod(name, attrs.FileMode().Perm()); err != nil {
				return err
			}
		}

		if attrFlags.Acmodtime {
			if err := lf.root.Chtimes(name, attrs.AccessTime(), attrs.ModTime()); err != nil {
				return err
			}
		}

		if attrFlags.UidGid {
			if err := lf.root.Chown(name, int(attrs.UID), int(attrs.GID)); err != nil {
				return err
			}
		}

		return nil
	}

	// Resolve the file once and operate on the resulting descriptor, so
	// the size change applies to exactly the file the name resolved to,
	// even if a directory entry is swapped afterwards.
	f, err := lf.root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(int64(r.Attributes().Size)); err != nil {
		return err
	}

	attrs := r.Attributes()

	if attrFlags.Permissions {
		if err := f.Chmod(attrs.FileMode().Perm()); err != nil {
			return err
		}
	}
	if attrFlags.Acmodtime {
		// os.File has no descriptor-based times API on every platform;
		// the Root resolution still cannot follow a link outside the root.
		if err := lf.root.Chtimes(name, attrs.AccessTime(), attrs.ModTime()); err != nil {
			return err
		}
	}
	if attrFlags.UidGid {
		if err := f.Chown(int(attrs.UID), int(attrs.GID)); err != nil {
			return err
		}
	}

	return nil
}

func (lf *LocalFile) rename(r *Request, posix bool) error {
	oldname, err := rel(r.Filepath)
	if err != nil {
		return err
	}
	newname, err := rel(r.Target)
	if err != nil {
		return err
	}

	// SFTP-v2 Rename must fail if newpath exists, unlike the POSIX
	// renameat syscall. Both names are resolved by the kernel relative
	// to the root directory handle; a symlink swapped in by another
	// goroutine at either end cannot redirect the operation outside it.
	if !posix {
		switch _, err := lf.root.Lstat(newname); {
		case err == nil:
			return &fs.PathError{Op: "rename", Path: newname, Err: os.ErrExist}
		case errors.Is(err, fs.ErrNotExist):
			// Expected: the destination is free.
		default:
			return err
		}
	}

	return lf.root.Rename(oldname, newname)
}

func (lf *LocalFile) remove(r *Request, dir bool) error {
	name, err := rel(r.Filepath)
	if err != nil {
		return err
	}

	info, err := lf.root.Lstat(name)
	if err != nil {
		return err
	}

	// Rmdir only removes directories; Remove only removes non-directories,
	// matching the in-memory reference backend and the SFTP-v2 distinction
	// between SSH_FXP_RMDIR and SSH_FXP_REMOVE. The unlink itself is
	// performed by the kernel against the same confined root, so a swap
	// after this Lstat can only cause the unlink to fail, never to remove
	// an outside entry.
	if dir != info.IsDir() {
		if dir {
			return &fs.PathError{Op: "rmdir", Path: name, Err: syscall.ENOTDIR}
		}
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.EISDIR}
	}

	return lf.root.Remove(name)
}

func (lf *LocalFile) mkdir(r *Request) error {
	name, err := rel(r.Filepath)
	if err != nil {
		return err
	}

	perm := os.FileMode(0o755)
	if r.AttrFlags().Permissions {
		perm = r.Attributes().FileMode().Perm()
	}

	return lf.root.Mkdir(name, perm)
}

func (lf *LocalFile) link(r *Request) error {
	oldname, err := rel(r.Filepath)
	if err != nil {
		return err
	}
	newname, err := rel(r.Target)
	if err != nil {
		return err
	}

	// Both the existing file and the new directory entry are resolved
	// inside the root by linkat/CreateHardLink; neither end can be made
	// to name an outside file by racing symlink or directory swaps.
	return lf.root.Link(oldname, newname)
}

func (lf *LocalFile) symlink(r *Request) error {
	// NOTE: r.Filepath is the link target (the contents of the link),
	// and r.Target is the path of the link itself.
	target := r.Filepath
	linkname, err := rel(r.Target)
	if err != nil {
		return err
	}

	if path.IsAbs(target) {
		// [os.Root] would refuse to follow an absolute link anyway,
		// but reject it at creation time so the result is unambiguous.
		return &fs.PathError{Op: "symlink", Path: linkname, Err: syscall.EPERM}
	}

	// Relative targets are stored as given: links that remain inside the
	// root resolve normally, while any target that resolves outside is
	// rejected at access time by the confined resolution in [os.Root].
	return lf.root.Symlink(filepath.FromSlash(target), linkname)
}

func (lf *LocalFile) Filelist(r *Request) (ListerAt, error) {
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "List":
		f, err := lf.root.Open(name)
		if err != nil {
			return nil, err
		}

		dirents, readErr := f.ReadDir(-1)
		f.Close()

		infos := make([]os.FileInfo, 0, len(dirents))
		for _, dirent := range dirents {
			info, err := dirent.Info()
			if err != nil {
				return nil, err
			}
			infos = append(infos, info)
		}

		la := listerat(infos)
		if readErr != nil {
			return la, readErr
		}
		return la, nil

	case "Stat":
		info, err := lf.root.Stat(name)
		if err != nil {
			return nil, err
		}
		return listerat{info}, nil
	}

	return nil, errors.New("unsupported")
}

func (lf *LocalFile) Lstat(r *Request) (ListerAt, error) {
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}

	info, err := lf.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	return listerat{info}, nil
}

func (lf *LocalFile) Readlink(name string) (string, error) {
	linkname, err := rel(name)
	if err != nil {
		return "", err
	}

	target, err := lf.root.Readlink(linkname)
	if err != nil {
		return "", err
	}

	// Report the link contents in POSIX style, as stored.
	return filepath.ToSlash(target), nil
}
