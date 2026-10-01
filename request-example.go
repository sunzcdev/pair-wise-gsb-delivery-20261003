package sftp

// This serves as an example of how to implement the request server handler as
// well as a dummy backend for testing. It implements an in-memory backend that
// works as a very simple filesystem with simple flat key-value lookup system.

import (
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxSymlinkFollows = 5

var errTooManySymlinks = errors.New("too many symbolic links")

// InMemHandler returns a Handlers object with the test handlers.
func InMemHandler() Handlers {
	root := &root{
		rootFile: &memFile{name: "/", modtime: time.Now(), isdir: true},
		files:    make(map[string]*memFile),
	}
	return Handlers{root, root, root, root}
}

// Example Handlers
func (fs *root) Fileread(r *Request) (io.ReaderAt, error) {
	flags := r.Pflags()
	if !flags.Read {
		// sanity check
		return nil, os.ErrInvalid
	}

	return fs.OpenFile(r)
}

func (fs *root) Filewrite(r *Request) (io.WriterAt, error) {
	flags := r.Pflags()
	if !flags.Write {
		// sanity check
		return nil, os.ErrInvalid
	}

	return fs.OpenFile(r)
}

func (fs *root) OpenFile(r *Request) (WriterAtReaderAt, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing

	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.openfile(r.Filepath, r.Flags)
}

func (fs *root) putfile(pathname string, file *memFile) error {
	pathname, err := fs.canonName(pathname)
	if err != nil {
		return err
	}

	if !strings.HasPrefix(pathname, "/") {
		return os.ErrInvalid
	}

	if _, err := fs.lfetch(pathname); err != os.ErrNotExist {
		return os.ErrExist
	}

	file.name = pathname
	fs.files[pathname] = file

	return nil
}

func (fs *root) openfile(pathname string, flags uint32) (*memFile, error) {
	pflags := newFileOpenFlags(flags)

	file, err := fs.fetch(pathname)
	if err == os.ErrNotExist {
		if !pflags.Creat {
			return nil, os.ErrNotExist
		}

		var count int
		// You can create files through dangling symlinks.
		link, err := fs.lfetch(pathname)
		for err == nil && link.symlink != "" {
			if pflags.Excl {
				// unless you also passed in O_EXCL
				return nil, os.ErrInvalid
			}

			if count++; count > maxSymlinkFollows {
				return nil, errTooManySymlinks
			}

			pathname = link.symlink
			link, err = fs.lfetch(pathname)
		}

		file := &memFile{
			modtime: time.Now(),
		}

		if err := fs.putfile(pathname, file); err != nil {
			return nil, err
		}

		return file, nil
	}

	if err != nil {
		return nil, err
	}

	if pflags.Creat && pflags.Excl {
		return nil, os.ErrExist
	}

	if file.IsDir() {
		return nil, os.ErrInvalid
	}

	if pflags.Trunc {
		if err := file.Truncate(0); err != nil {
			return nil, err
		}
	}

	return file, nil
}

func (fs *root) Filecmd(r *Request) error {
	if fs.mockErr != nil {
		return fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing

	fs.mu.Lock()
	defer fs.mu.Unlock()

	switch r.Method {
	case "Setstat":
		file, err := fs.openfile(r.Filepath, sshFxfWrite)
		if err != nil {
			return err
		}

		if r.AttrFlags().Size {
			return file.Truncate(int64(r.Attributes().Size))
		}

		return nil

	case "Rename":
		// SFTP-v2: "It is an error if there already exists a file with the name specified by newpath."
		// This varies from the POSIX specification, which allows limited replacement of target files.
		if fs.exists(r.Target) {
			return os.ErrExist
		}

		return fs.rename(r.Filepath, r.Target)

	case "Rmdir":
		return fs.rmdir(r.Filepath)

	case "Remove":
		// IEEE 1003.1 remove explicitly can unlink files and remove empty directories.
		// We use instead here the semantics of unlink, which is allowed to be restricted against directories.
		return fs.unlink(r.Filepath)

	case "Mkdir":
		return fs.mkdir(r.Filepath)

	case "Link":
		return fs.link(r.Filepath, r.Target)

	case "Symlink":
		// NOTE: r.Filepath is the target, and r.Target is the linkpath.
		return fs.symlink(r.Filepath, r.Target)
	}

	return errors.New("unsupported")
}

func (fs *root) rename(oldpath, newpath string) error {
	file, err := fs.lfetch(oldpath)
	if err != nil {
		return err
	}

	newpath, err = fs.canonName(newpath)
	if err != nil {
		return err
	}

	if !strings.HasPrefix(newpath, "/") {
		return os.ErrInvalid
	}

	target, err := fs.lfetch(newpath)
	if err != os.ErrNotExist {
		if target == file {
			// IEEE 1003.1: if oldpath and newpath are the same directory entry,
			// then return no error, and perform no further action.
			return nil
		}

		switch {
		case file.IsDir():
			// IEEE 1003.1: if oldpath is a directory, and newpath exists,
			// then newpath must be a directory, and empty.
			// It is to be removed prior to rename.
			if err := fs.rmdir(newpath); err != nil {
				return err
			}

		case target.IsDir():
			// IEEE 1003.1: if oldpath is not a directory, and newpath exists,
			// then newpath may not be a directory.
			return syscall.EISDIR
		}
	}

	fs.files[newpath] = file

	if file.IsDir() {
		dirprefix := file.name + "/"

		for name, file := range fs.files {
			if strings.HasPrefix(name, dirprefix) {
				newname := path.Join(newpath, strings.TrimPrefix(name, dirprefix))

				fs.files[newname] = file
				file.name = newname
				delete(fs.files, name)
			}
		}
	}

	file.name = newpath
	delete(fs.files, oldpath)

	return nil
}

func (fs *root) PosixRename(r *Request) error {
	if fs.mockErr != nil {
		return fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing

	fs.mu.Lock()
	defer fs.mu.Unlock()

	return fs.rename(r.Filepath, r.Target)
}

func (fs *root) StatVFS(r *Request) (*StatVFS, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}

	return getStatVFSForPath(r.Filepath)
}

func (fs *root) mkdir(pathname string) error {
	dir := &memFile{
		modtime: time.Now(),
		isdir:   true,
	}

	return fs.putfile(pathname, dir)
}

func (fs *root) rmdir(pathname string) error {
	// IEEE 1003.1: If pathname is a symlink, then rmdir should fail with ENOTDIR.
	dir, err := fs.lfetch(pathname)
	if err != nil {
		return err
	}

	if !dir.IsDir() {
		return syscall.ENOTDIR
	}

	// use the dir‘s internal name not the pathname we passed in.
	// the dir.name is always the canonical name of a directory.
	pathname = dir.name

	for name := range fs.files {
		if path.Dir(name) == pathname {
			return errors.New("directory not empty")
		}
	}

	delete(fs.files, pathname)

	return nil
}

func (fs *root) link(oldpath, newpath string) error {
	file, err := fs.lfetch(oldpath)
	if err != nil {
		return err
	}

	if file.IsDir() {
		return errors.New("hard link not allowed for directory")
	}

	return fs.putfile(newpath, file)
}

// symlink() creates a symbolic link named `linkpath` which contains the string `target`.
// NOTE! This would be called with `symlink(req.Filepath, req.Target)` due to different semantics.
func (fs *root) symlink(target, linkpath string) error {
	link := &memFile{
		modtime: time.Now(),
		symlink: target,
	}

	return fs.putfile(linkpath, link)
}

func (fs *root) unlink(pathname string) error {
	// does not follow symlinks!
	file, err := fs.lfetch(pathname)
	if err != nil {
		return err
	}

	if file.IsDir() {
		// IEEE 1003.1: implementations may opt out of allowing the unlinking of directories.
		// SFTP-v2: SSH_FXP_REMOVE may not remove directories.
		return os.ErrInvalid
	}

	// DO NOT use the file’s internal name.
	// because of hard-links files cannot have a single canonical name.
	delete(fs.files, pathname)

	return nil
}

type listerat []os.FileInfo

// Modeled after strings.Reader's ReadAt() implementation
func (f listerat) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	var n int
	if offset >= int64(len(f)) {
		return 0, io.EOF
	}
	n = copy(ls, f[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

func (fs *root) Filelist(r *Request) (ListerAt, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing

	fs.mu.Lock()
	defer fs.mu.Unlock()

	switch r.Method {
	case "List":
		files, err := fs.readdir(r.Filepath)
		if err != nil {
			return nil, err
		}
		return listerat(files), nil

	case "Stat":
		file, err := fs.fetch(r.Filepath)
		if err != nil {
			return nil, err
		}
		return listerat{file}, nil
	}

	return nil, errors.New("unsupported")
}

func (fs *root) readdir(pathname string) ([]os.FileInfo, error) {
	dir, err := fs.fetch(pathname)
	if err != nil {
		return nil, err
	}

	if !dir.IsDir() {
		return nil, syscall.ENOTDIR
	}

	var files []os.FileInfo

	for name, file := range fs.files {
		if path.Dir(name) == dir.name {
			files = append(files, file)
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })

	return files, nil
}

func (fs *root) Readlink(pathname string) (string, error) {
	file, err := fs.lfetch(pathname)
	if err != nil {
		return "", err
	}

	if file.symlink == "" {
		return "", os.ErrInvalid
	}

	return file.symlink, nil
}

// implements LstatFileLister interface
func (fs *root) Lstat(r *Request) (ListerAt, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing

	fs.mu.Lock()
	defer fs.mu.Unlock()

	file, err := fs.lfetch(r.Filepath)
	if err != nil {
		return nil, err
	}
	return listerat{file}, nil
}

// In memory file-system-y thing that the Handlers live on
type root struct {
	rootFile *memFile
	mockErr  error

	mu    sync.Mutex
	files map[string]*memFile
}

// Set a mocked error that the next handler call will return.
// Set to nil to reset for no error.
func (fs *root) returnErr(err error) {
	fs.mockErr = err
}

func (fs *root) lfetch(path string) (*memFile, error) {
	if path == "/" {
		return fs.rootFile, nil
	}

	file, ok := fs.files[path]
	if file == nil {
		if ok {
			delete(fs.files, path)
		}

		return nil, os.ErrNotExist
	}

	return file, nil
}

// canonName returns the “canonical” name of a file, that is:
// if the directory of the pathname is a symlink, it follows that symlink to the valid directory name.
// this is relatively easy, since `dir.name` will be the only valid canonical path for a directory.
func (fs *root) canonName(pathname string) (string, error) {
	dirname, filename := path.Dir(pathname), path.Base(pathname)

	dir, err := fs.fetch(dirname)
	if err != nil {
		return "", err
	}

	if !dir.IsDir() {
		return "", syscall.ENOTDIR
	}

	return path.Join(dir.name, filename), nil
}

func (fs *root) exists(path string) bool {
	path, err := fs.canonName(path)
	if err != nil {
		return false
	}

	_, err = fs.lfetch(path)

	return err != os.ErrNotExist
}

func (fs *root) fetch(pathname string) (*memFile, error) {
	file, err := fs.lfetch(pathname)
	if err != nil {
		return nil, err
	}

	var count int
	for file.symlink != "" {
		if count++; count > maxSymlinkFollows {
			return nil, errTooManySymlinks
		}

		linkTarget := file.symlink
		if !path.IsAbs(linkTarget) {
			linkTarget = path.Join(path.Dir(file.name), linkTarget)
		}

		file, err = fs.lfetch(linkTarget)
		if err != nil {
			return nil, err
		}
	}

	return file, nil
}

// Implements os.FileInfo, io.ReaderAt and io.WriterAt interfaces.
// These are the 3 interfaces necessary for the Handlers.
// Implements the optional interface TransferError.
type memFile struct {
	name    string
	modtime time.Time
	symlink string
	isdir   bool

	mu      sync.RWMutex
	content []byte
	// allocated records the backing regions of content. Gaps between these
	// regions are holes and read back as zero bytes without being "allocated",
	// while an explicitly written run of zero bytes stays allocated and must
	// therefore still be transferred.
	allocated []fileRangeMem
	err       error
}

type fileRangeMem struct {
	offset int64
	length int64
}

// SparseCapabilities advertises full sparse support for the in-memory backend.
func (fs *root) SparseCapabilities() SparseCaps {
	return SparseCaps{Discover: true, Allocate: true}
}

// markAllocated records [offset, offset+length) as backed storage, merging it
// with adjacent/overlapping existing regions. Must be called holding f.mu.
func (f *memFile) markAllocated(offset, length int64) {
	if length <= 0 {
		return
	}

	start := offset
	end := offset + length

	var merged []fileRangeMem

	for _, r := range f.allocated {
		rEnd := r.offset + r.length

		if rEnd < start {
			merged = append(merged, r)
			continue
		}

		if r.offset > end {
			merged = append(merged, fileRangeMem{start, end - start})
			start, end = r.offset, rEnd
			continue
		}

		if r.offset < start {
			start = r.offset
		}

		if rEnd > end {
			end = rEnd
		}
	}

	merged = append(merged, fileRangeMem{start, end - start})
	f.allocated = merged
}

// isAllocated reports whether off falls inside a backed region. Must be
// called holding f.mu.
func (f *memFile) isAllocated(off int64) bool {
	for _, r := range f.allocated {
		if off >= r.offset && off < r.offset+r.length {
			return true
		}
	}

	return false
}

// QueryAllocatedRanges implements AllocatedRanger for protocol verification.
// The returned nextOffset is the start of the following hole, or the logical
// file size when no allocated data remains.
func (f *memFile) QueryAllocatedRanges(offset uint64, count uint32) ([]FileRange, uint64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if f.err != nil {
		return nil, 0, f.err
	}

	size := f.size()

	if int64(offset) >= size {
		return nil, uint64(size), nil
	}

	var ranges []FileRange

	cursor := int64(offset)

	for _, r := range f.allocated {
		rStart := r.offset
		rEnd := r.offset + r.length

		if rEnd <= cursor {
			continue // region entirely before cursor
		}

		if rStart > cursor {
			if len(ranges) > 0 {
				// Gap before this region; stop here so the caller skips it.
				return ranges, uint64(rStart), nil
			}

			// cursor sits inside a hole: jump to the start of this region.
			cursor = rStart
		}

		if rEnd > size {
			rEnd = size
		}

		ranges = append(ranges, FileRange{
			Offset: uint64(cursor),
			Length: uint64(rEnd - cursor),
		})
		cursor = rEnd

		if uint32(len(ranges)) >= count {
			break
		}
	}

	if len(ranges) == 0 {
		return nil, uint64(size), nil
	}

	// After the last returned range the next byte is a hole (or EOF).
	lastEnd := int64(ranges[len(ranges)-1].End())
	if lastEnd >= size {
		return ranges, uint64(size), nil
	}

	return ranges, uint64(lastEnd), nil
}

// AllocateRegions implements RegionAllocator for protocol verification. It
// marks exactly the given regions as backed storage; gaps remain holes.
func (f *memFile) AllocateRegions(ranges []FileRange) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	for _, r := range ranges {
		end := int64(r.Offset + r.Length)

		if end > f.size() {
			f.grow(end - f.size())
		}

		f.markAllocated(int64(r.Offset), int64(r.Length))
	}

	return nil
}

// These are helper functions, they must be called while holding the memFile.mu mutex
func (f *memFile) size() int64  { return int64(len(f.content)) }
func (f *memFile) grow(n int64) { f.content = append(f.content, make([]byte, n)...) }

// Have memFile fulfill os.FileInfo interface
func (f *memFile) Name() string { return path.Base(f.name) }
func (f *memFile) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.size()
}
func (f *memFile) Mode() os.FileMode {
	if f.isdir {
		return os.FileMode(0755) | os.ModeDir
	}
	if f.symlink != "" {
		return os.FileMode(0777) | os.ModeSymlink
	}
	return os.FileMode(0644)
}
func (f *memFile) ModTime() time.Time { return f.modtime }
func (f *memFile) IsDir() bool        { return f.isdir }
func (f *memFile) Sys() any {
	return fakeFileInfoSys()
}

func (f *memFile) ReadAt(b []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return 0, f.err
	}

	if off < 0 {
		return 0, errors.New("memFile.ReadAt: negative offset")
	}

	if off >= f.size() {
		return 0, io.EOF
	}

	n := copy(b, f.content[off:])
	if n < len(b) {
		return n, io.EOF
	}

	return n, nil
}

func (f *memFile) WriteAt(b []byte, off int64) (int, error) {
	// fmt.Println(string(p), off)
	// mimic write delays, should be optional
	time.Sleep(time.Microsecond * time.Duration(len(b)))

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return 0, f.err
	}

	grow := int64(len(b)) + off - f.size()
	if grow > 0 {
		f.grow(grow)
	}

	// Every written byte — including explicit zero bytes — is backed storage.
	f.markAllocated(off, int64(len(b)))

	return copy(f.content[off:], b), nil
}

func (f *memFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	grow := size - f.size()
	if grow <= 0 {
		f.content = f.content[:size]
	} else {
		f.grow(grow)
	}

	// Drop regions past the new size. Growing must not mark the extension as
	// allocated: the new tail is a logical hole.
	var clipped []fileRangeMem

	for _, r := range f.allocated {
		rEnd := r.offset + r.length
		if r.offset >= size {
			continue
		}

		if rEnd > size {
			r.length = size - r.offset
		}

		clipped = append(clipped, r)
	}

	f.allocated = clipped

	return nil
}

func (f *memFile) TransferError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.err = err
}
