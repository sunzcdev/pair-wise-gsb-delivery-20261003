package common

import (
	"os"
	"runtime"
	"time"
)

// MaxMmapStep is the largest step that can be taken when remapping the mmap.
const MaxMmapStep = 1 << 30 // 1GB

// Version represents the default data file format version.
const Version uint32 = 2

// VersionPageChecksum represents the data file format version used by
// databases created with page checksums enabled. It is only written to
// newly created databases; existing version 2 databases are never
// upgraded in place. Binaries which only understand Version reject such
// files with a version mismatch error.
const VersionPageChecksum uint32 = 3

// Magic represents a marker value to indicate that a file is a Bolt DB.
const Magic uint32 = 0xED0CDAED

const PgidNoFreelist Pgid = 0xffffffffffffffff

// IgnoreNoSync specifies whether the NoSync field of a DB is ignored when
// syncing changes to a file.  This is required as some operating systems,
// such as OpenBSD, do not have a unified buffer cache (UBC) and writes
// must be synchronized using the msync(2) syscall.
const IgnoreNoSync = runtime.GOOS == "openbsd"

// Default values if not set in a DB instance.
const (
	DefaultMaxBatchSize  int = 1000
	DefaultMaxBatchDelay     = 10 * time.Millisecond
	DefaultAllocSize         = 16 * 1024 * 1024
)

// DefaultPageSize is the default page size for db which is set to the OS page size.
var DefaultPageSize = os.Getpagesize()

// Txid represents the internal transaction identifier.
type Txid uint64
