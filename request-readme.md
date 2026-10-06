# Request Based SFTP API

The request based API allows for custom backends in a way similar to the http
package. In order to create a backend you need to implement 4 handler
interfaces; one for reading, one for writing, one for misc commands and one for
listing files. Each has 1 required method and in each case those methods take
the Request as the only parameter and they each return something different.
These 4 interfaces are enough to handle all the SFTP traffic in a simplified
manner.

## Local filesystem backend

Use `NewLocalFileHandler(root)` to serve a local directory without path-prefix
checks. The handler keeps the root directory open and performs operations
through directory-relative file handles. Both endpoints of renames and links
are resolved through those handles, so replacing directories during a request
cannot move the operation outside the root.

Symbolic links are followed when their lexical target remains inside the
virtual root. Absolute SFTP symlink targets are interpreted as virtual paths
below `/`; links that would target outside root are rejected.

The race-resistant implementation is supported on Unix and Windows. Other
platforms fail command operations instead of presenting a weaker containment
guarantee.

The Request structure has 5 public fields which you will deal with.

- Method (string) - string name of incoming call
- Filepath (string) - POSIX path of file to act on
- Flags (uint32) - 32bit bitmask value of file open/create flags
- Attrs ([]byte) - byte string of file attribute data
- Target (string) - target path for renames and sym-links

Below are the methods and a brief description of what they need to do.

### Fileread(*Request) (io.Reader, error)

Handler for "Get" method and returns an io.Reader for the file which the server
then sends to the client.

### Filewrite(*Request) (io.Writer, error)

Handler for "Put" method and returns an io.Writer for the file which the server
then writes the uploaded file to. The file opening "pflags" are currently
preserved in the Request.Flags field as a 32bit bitmask value. See the [SFTP
spec](https://filezilla-project.org/specs/draft-ietf-secsh-filexfer-02.txt#section-6.3) for
details.

###    Filecmd(*Request) error

Handles "SetStat", "Rename", "Rmdir", "Mkdir"  and "Symlink" methods. Makes the
appropriate changes and returns nil for success or an filesystem like error
(eg. os.ErrNotExist). The attributes are currently propagated in their raw form
([]byte) and will need to be unmarshalled to be useful. See the respond method
on sshFxpSetstatPacket for example of you might want to do this.

### Fileinfo(*Request) ([]os.FileInfo, error)

Handles "List", "Stat", "Readlink" methods. Gathers/creates FileInfo structs
with the data on the files and returns in a list (list of 1 for Stat and
Readlink).


## TODO

- Add support for API users to see trace/debugging info of what is going on
inside SFTP server.
- Unmarshal the file attributes into a structure on the Request object.
