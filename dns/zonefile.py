# Copyright (C) Dnspython Contributors, see LICENSE for text of ISC license

# Copyright (C) 2003-2007, 2009-2011 Nominum, Inc.
#
# Permission to use, copy, modify, and distribute this software and its
# documentation for any purpose with or without fee is hereby granted,
# provided that the above copyright notice and this permission notice
# appear in all copies.
#
# THE SOFTWARE IS PROVIDED "AS IS" AND NOMINUM DISCLAIMS ALL WARRANTIES
# WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
# MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL NOMINUM BE LIABLE FOR
# ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
# WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
# ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT
# OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.

"""DNS Zones."""

import dataclasses
import os
import re
import sys
from collections.abc import Iterable
from typing import Any, cast

import dns.exception
import dns.grange
import dns.name
import dns.node
import dns.rdata
import dns.rdataclass
import dns.rdatatype
import dns.rdtypes.ANY.SOA
import dns.rrset
import dns.tokenizer
import dns.transaction
import dns.ttl


class UnknownOrigin(dns.exception.DNSException):
    """Unknown origin"""


class CNAMEAndOtherData(dns.exception.DNSException):
    """A node has a CNAME and other data"""


class IncludeError(dns.exception.SyntaxError):
    """A ``$INCLUDE`` directive could not be honored.

    :ivar str reason: Why the directive could not be honored.
    :ivar list[str] chain: The include chain, from the outermost file to the
        file which could not be read.  Each item is a
        ``filename:line`` location, except that the last one names the file
        whose inclusion failed.

    The string form of the exception is the reason followed by the chain,
    one location per line, with the first location prefixed by ``"in "`` and
    the others indented:
    ::

        $INCLUDE of 'foo.zone' would create a cycle
        in example.zone:10
           foo.zone:3
             example.zone
    """

    def __init__(self, reason: str, chain: Iterable[str] = ()):
        self.reason = reason
        self.chain = list(chain)
        super().__init__(reason)

    def __str__(self) -> str:
        if not self.chain:
            return self.reason
        return f"{self.reason}\n{_format_include_chain(self.chain)}"


class IncludeNotAllowed(IncludeError):
    """A ``$INCLUDE`` would read a file outside the allowed directories."""


class IncludeCycle(IncludeError):
    """A ``$INCLUDE`` would create a cycle of includes."""


class IncludeTooDeep(IncludeError):
    """``$INCLUDE`` directives are nested more deeply than is allowed."""


def _check_cname_and_other_data(txn, name, rdataset):
    rdataset_kind = dns.node.NodeKind.classify_rdataset(rdataset)
    node = txn.get_node(name)
    if node is None:
        # empty nodes are neutral.
        return
    node_kind = node.classify()
    if (
        node_kind == dns.node.NodeKind.CNAME
        and rdataset_kind == dns.node.NodeKind.REGULAR
    ):
        raise CNAMEAndOtherData("rdataset type is not compatible with a CNAME node")
    elif (
        node_kind == dns.node.NodeKind.REGULAR
        and rdataset_kind == dns.node.NodeKind.CNAME
    ):
        raise CNAMEAndOtherData(
            "CNAME rdataset is not compatible with a regular data node"
        )
    # Otherwise at least one of the node and the rdataset is neutral, so
    # adding the rdataset is ok


SavedStateType = tuple[
    dns.tokenizer.Tokenizer,
    dns.name.Name | None,  # current_origin
    dns.name.Name | None,  # last_name
    Any | None,  # current_file
    int,  # last_ttl
    bool,  # last_ttl_known
    int,  # default_ttl
    bool,  # default_ttl_known
    bool,  # default_ttl_from_soa
]


def _upper_dollarize(s):
    s = s.upper()
    if not s.startswith("$"):
        s = "$" + s
    return s


# A $GENERATE modifier: ${offset[,width[,base]]}
#
# BIND formats each substitution into a 128 byte buffer and rejects it if it
# does not fit, so the width cannot usefully exceed 127.  Without a limit, a
# short line could ask for an arbitrarily long zero-padded string.
_MAX_GENERATE_WIDTH = 127
_generate_modifier = re.compile(r"{([+-]?\d+)(?:,(\d+)(?:,([doxXnN]))?)?}")


def _generate_nibbles(value: int, width: int, base: str) -> str:
    """Format *value* as dot-separated hexadecimal nibbles, least significant
    nibble first, as BIND does.  All of the nibbles of *value* are output,
    and zero nibbles are added until the output is at least *width*
    characters long."""
    digits = "0123456789ABCDEF" if base == "N" else "0123456789abcdef"
    text = ""
    while True:
        text += digits[value & 0xF]
        value >>= 4
        if width > 0:
            width -= 1
        if width > 0 or value != 0:
            text += "."
            if width > 0:
                width -= 1
        if value == 0 and width == 0:
            return text


def _generate_text(template: str, counter: int) -> str:
    """Substitute the $GENERATE iterator *counter* into *template*.

    Each "$" is replaced by the counter, optionally modified by a following
    ${offset[,width[,base]]}.  "$$" is a literal "$", and a backslash escape
    such as "\\$" is copied unchanged, as in BIND.
    """
    text = ""
    i = 0
    while i < len(template):
        c = template[i]
        if c == "\\":
            text += template[i : i + 2]
            i += 2
        elif c != "$":
            text += c
            i += 1
        elif template.startswith("$", i + 1):
            text += "$"
            i += 2
        else:
            i += 1
            offset = 0
            width = 0
            base = "d"
            if template.startswith("{", i):
                match = _generate_modifier.match(template, i)
                if match is None:
                    raise dns.exception.SyntaxError("invalid $GENERATE modifier")
                offset = int(match.group(1))
                if match.group(2) is not None:
                    width = int(match.group(2))
                    if width > _MAX_GENERATE_WIDTH:
                        raise dns.exception.SyntaxError("$GENERATE width is too large")
                if match.group(3) is not None:
                    base = match.group(3)
                i = match.end()
            value = counter + offset
            if base in "nN":
                if value < 0:
                    raise dns.exception.SyntaxError(
                        "negative $GENERATE value in nibble mode"
                    )
                text += _generate_nibbles(value, width, base)
            else:
                text += format(value, base).zfill(width)
    return text


# The default maximum number of nested $INCLUDE directives.
DEFAULT_MAX_INCLUDE_DEPTH = 10

# The default encoding of files read by $INCLUDE directives.
DEFAULT_INCLUDE_ENCODING = "utf-8"


def _is_synthetic_filename(filename: str) -> bool:
    """Is *filename* a placeholder such as ``"<string>"`` rather than a path?"""
    return filename.startswith("<") and filename.endswith(">")


def _canonical_path(filename: str | os.PathLike[str] | None) -> str | None:
    """Return the canonical absolute form of *filename*.

    ``None`` is returned for names which are not paths, i.e. the synthetic
    names like ``"<string>"`` which the tokenizer uses for input that did not
    come from a file.  The path is canonicalized with
    :py:func:`os.path.realpath`, so ``..`` components and symbolic links are
    resolved.
    """
    if filename is None:
        return None
    text = os.fspath(filename)
    if _is_synthetic_filename(text):
        return None
    return os.path.realpath(text)


@dataclasses.dataclass(frozen=True)
class IncludeOrigin:
    """The place where an ``$INCLUDE`` directive was found.

    *filename* is the name of the including file as reported in diagnostics,
    *line* is the line number of the directive itself, and *path* is the
    canonical path of the including file, or ``None`` if the including input
    is not a file (e.g. it is a string).
    """

    filename: str
    line: int
    path: str | None = None

    def __str__(self) -> str:
        return f"{self.filename}:{self.line}"


@dataclasses.dataclass(frozen=True)
class IncludeSource:
    """The resolved location of the file named by an ``$INCLUDE`` directive.

    *filename* is the canonical absolute path of the file, and *directory* is
    the canonical directory which contains it.  The latter is the base
    against which relative ``$INCLUDE`` names in the file are resolved.
    """

    filename: str
    directory: str


@dataclasses.dataclass(frozen=True)
class _IncludeFrame:
    source: IncludeSource
    file: Any
    origin: IncludeOrigin | None


def _format_include_chain(chain: Iterable[str], indent: str = "") -> str:
    """Format an include chain for a diagnostic.

    The first location is where reading started and the others are the nested
    directives, so the latter are indented to make the chain easy to read.
    """
    locations = list(chain)
    if not locations:
        return ""
    lines = [f"{indent}in {locations[0]}"]
    for depth, location in enumerate(locations[1:], start=1):
        lines.append(f"{indent}{'  ' * depth}{location}")
    return "\n".join(lines)


class IncludeResolver:
    """An explicit source resolver for ``$INCLUDE`` directives.

    A resolver is responsible for turning the filename written in an
    ``$INCLUDE`` directive into a file to read.  Relative filenames are
    resolved against the directory of the file which contains the directive,
    rather than against the process working directory, so an included file's
    own includes are found next to it.  A resolver also detects an include
    which leads back to a file which is already being read, and limits how
    deeply includes may nest.

    If *directories* is given, then it confines ``$INCLUDE`` to those
    directories: only files whose canonical path (i.e. after resolving ``..``
    components and symbolic links) is inside one of them may be included.  An
    absolute path is then only usable if it points inside a declared
    directory, and neither a parent directory reference nor a symbolic link
    can lead outside of one.  If *directories* is ``None``, no such
    confinement is done, which is only appropriate when the zone file is
    trusted; :py:class:`Reader` uses a confined resolver by default.

    A resolver keeps state, namely which files are currently being read, and
    so a resolver instance must be used by only one :py:class:`Reader` at a
    time.  The same file may be included any number of times, sequentially or
    in different branches of the include tree; only including a file which is
    already being read, i.e. one which is further up the current include
    stack, is an error.

    :param directories: The directories ``$INCLUDE`` may read from, or
        ``None`` to allow any file.
    :type directories: Iterable[str | os.PathLike[str]] or ``None``
    :param int max_depth: The maximum number of nested includes.  The
        outermost file is not counted, so a value of 0 prohibits includes
        and a value of 1 allows the outermost file to include files which
        themselves do not include anything.
    :param str encoding: The encoding of the included files; the default is
        ``"utf-8"``.
    """

    def __init__(
        self,
        directories: Iterable[str | os.PathLike[str]] | None = None,
        max_depth: int = DEFAULT_MAX_INCLUDE_DEPTH,
        encoding: str = DEFAULT_INCLUDE_ENCODING,
    ):
        if max_depth < 0:
            raise ValueError("max_depth must not be negative")
        if directories is None:
            self._directories: tuple[str, ...] | None = None
        else:
            canonical: list[str] = []
            for directory in directories:
                path = os.path.realpath(os.fspath(directory))
                if path not in canonical:
                    canonical.append(path)
            self._directories = tuple(canonical)
        self._max_depth = max_depth
        self._encoding = encoding
        self._root: IncludeSource | None = None
        self._stack: list[_IncludeFrame] = []

    @property
    def directories(self) -> tuple[str, ...] | None:
        """The canonical directory names ``$INCLUDE`` may read from, or
        ``None`` if any file may be included."""
        return self._directories

    @property
    def max_depth(self) -> int:
        """The maximum number of nested includes."""
        return self._max_depth

    @property
    def depth(self) -> int:
        """The number of includes currently being read."""
        return len(self._stack)

    @property
    def root(self) -> IncludeSource | None:
        """The outermost file, as declared with :py:meth:`set_root`."""
        return self._root

    def current_path(self) -> str | None:
        """The canonical path of the file currently being read, if any.

        This is the innermost file in the include stack, or the outermost
        file if no include is being read.  ``None`` is returned if the input
        is not a file at all, e.g. it is a string.
        """
        if self._stack:
            return self._stack[-1].source.filename
        if self._root is not None:
            return self._root.filename
        return None

    def include_chain(self, where: IncludeOrigin | None = None) -> list[str]:
        """Return the include chain as a list of locations.

        The list starts with the location of the outermost ``$INCLUDE``
        directive being read and ends with the innermost.  If *where* is
        given, it is appended, which is how a diagnostic names the place
        which could not be processed.
        """
        chain = [
            str(frame.origin)
            if frame.origin is not None
            else frame.source.filename
            for frame in self._stack
        ]
        if where is not None:
            chain.append(str(where))
        return chain

    def set_root(self, filename: str | os.PathLike[str] | None) -> None:
        """Declare the outermost file being read.

        This lets the resolver recognize an include which leads back to the
        outermost file, and gives relative ``$INCLUDE`` names in input which
        is not a file itself a directory to be resolved against.  ``None``
        says the outermost input has no location of its own, in which case
        relative names are resolved against the current working directory.
        """
        path = _canonical_path(filename)
        if path is None:
            self._root = None
        else:
            self._root = IncludeSource(path, os.path.dirname(path))

    def reset(self) -> None:
        """Close any open included files and forget the include stack."""
        while self._stack:
            self._stack.pop().file.close()

    def _is_allowed(self, path: str) -> bool:
        if self._directories is None:
            return True
        normalized = os.path.normcase(path)
        for directory in self._directories:
            root = os.path.normcase(directory)
            try:
                if os.path.commonpath([normalized, root]) == root:
                    return True
            except ValueError:
                # The paths are on different drives (Windows).
                continue
        return False

    def _chain(self, origin: IncludeOrigin | None, tail: str) -> list[str]:
        chain = self.include_chain(origin)
        chain.append(tail)
        return chain

    def _base_directory(self, origin: IncludeOrigin | None) -> str:
        if origin is not None and origin.path is not None:
            return os.path.dirname(origin.path)
        if self._root is not None:
            return self._root.directory
        return os.getcwd()

    def resolve(self, name: str, origin: IncludeOrigin | None = None) -> IncludeSource:
        """Resolve the filename of an ``$INCLUDE`` directive.

        *name* is the filename as written in the directive.  A relative name
        is resolved against the directory of *origin*, i.e. of the file which
        contains the directive; if *origin* does not identify a file, then
        the directory of the file declared with :py:meth:`set_root` is used,
        or the current working directory if there is no such file either.

        :raises IncludeNotAllowed: if the file is outside of the declared
            directories.
        :returns: An :py:class:`IncludeSource`.
        """
        if name == "":
            raise IncludeError(
                "empty filename in $INCLUDE", self._chain(origin, "<empty>")
            )
        if os.path.isabs(name):
            candidate = name
        else:
            candidate = os.path.join(self._base_directory(origin), name)
        path = os.path.realpath(candidate)
        if not self._is_allowed(path):
            raise IncludeNotAllowed(
                f"$INCLUDE of '{name}' is outside the allowed directories",
                self._chain(origin, path),
            )
        return IncludeSource(path, os.path.dirname(path))

    def enter(
        self, name: str, origin: IncludeOrigin | None = None
    ) -> tuple[IncludeSource, Any]:
        """Resolve, check, and open the file of an ``$INCLUDE`` directive.

        *name* and *origin* are as for :py:meth:`resolve`.

        :raises IncludeNotAllowed: if the file is outside of the declared
            directories.
        :raises IncludeCycle: if the file is already being read.
        :raises IncludeTooDeep: if including the file would exceed the
            maximum depth.
        :raises IncludeError: if the file cannot be opened.
        :returns: A ``(source, file)`` tuple.  The file must be released with
            :py:meth:`leave`.
        """
        source = self.resolve(name, origin)
        key = os.path.normcase(source.filename)
        # Only files which are currently being read make a cycle.  A file
        # which has already been read to its end may be included again, and
        # the same file may be included from two different branches.
        reading = [frame.source.filename for frame in self._stack]
        if self._root is not None:
            reading.append(self._root.filename)
        if any(os.path.normcase(path) == key for path in reading):
            raise IncludeCycle(
                f"$INCLUDE of '{name}' would create a cycle",
                self._chain(origin, source.filename),
            )
        if self.depth >= self._max_depth:
            raise IncludeTooDeep(
                f"$INCLUDE nesting exceeds the maximum depth of {self._max_depth}",
                self._chain(origin, source.filename),
            )
        try:
            file = open(source.filename, encoding=self._encoding)
        except OSError as detail:
            raise IncludeError(
                f"cannot read the $INCLUDE file '{name}': {detail}",
                self._chain(origin, source.filename),
            ) from detail
        self._stack.append(_IncludeFrame(source, file, origin))
        return source, file

    def leave(self) -> None:
        """Finish reading the most recently entered include and close it."""
        if self._stack:
            self._stack.pop().file.close()


class Reader:
    """Read a DNS zone file into a transaction.

    ``$INCLUDE`` directives are handled by *include_resolver* if it is
    supplied; otherwise a resolver is created for the input.  It resolves
    relative filenames against the directory of the including file, confines
    includes to the directories named by *include_directories* (by default,
    the directory of the input file if it is a file), detects include cycles,
    and rejects nesting deeper than *max_include_depth*.  Pass
    ``IncludeResolver(None)`` as *include_resolver* to read a trusted zone
    file without confinement.
    """

    def __init__(
        self,
        tok: dns.tokenizer.Tokenizer,
        rdclass: dns.rdataclass.RdataClass,
        txn: dns.transaction.Transaction,
        allow_include: bool = False,
        allow_directives: bool | Iterable[str] = True,
        force_name: dns.name.Name | None = None,
        force_ttl: int | None = None,
        force_rdclass: dns.rdataclass.RdataClass | None = None,
        force_rdtype: dns.rdatatype.RdataType | None = None,
        default_ttl: int | None = None,
        rfc2308_ttl: bool = True,
        include_resolver: IncludeResolver | None = None,
        include_directories: Iterable[str | os.PathLike[str]] | None = None,
        max_include_depth: int = DEFAULT_MAX_INCLUDE_DEPTH,
    ):
        self.tok = tok
        self.zone_origin, self.relativize, _ = txn.manager.origin_information()
        self.current_origin = self.zone_origin
        self.last_ttl = 0
        self.last_ttl_known = False
        self.rfc2308_ttl = rfc2308_ttl
        self.default_ttl_from_soa = False
        if force_ttl is not None:
            default_ttl = force_ttl
        if default_ttl is None:
            self.default_ttl = 0
            self.default_ttl_known = False
        else:
            self.default_ttl = default_ttl
            self.default_ttl_known = True
        self.last_name = self.current_origin
        self.zone_rdclass = rdclass
        self.txn = txn
        self.saved_state: list[SavedStateType] = []
        self.current_file: Any | None = None
        if include_resolver is not None:
            self.include_resolver = include_resolver
        else:
            if include_directories is None:
                # The caller declared the boundary by naming the file to read,
                # so includes are confined to that file's directory.  Input
                # which is not a file, e.g. a string, has no directory to be
                # confined to, so it keeps the historical behavior of allowing
                # any file.  Pass include_directories or an IncludeResolver to
                # be explicit, or IncludeResolver(None) to read a trusted zone
                # file without confinement.
                root = _canonical_path(tok.filename)
                if root is not None:
                    include_directories = [os.path.dirname(root)]
            self.include_resolver = IncludeResolver(
                include_directories, max_depth=max_include_depth
            )
        # The file being read defines the root of the include tree, both for
        # cycle detection and as the base for relative $INCLUDE names when the
        # directive appears in input which is not itself a file.  Input with
        # no path of its own (e.g. a string) leaves any root the caller has
        # already declared alone.
        root = _canonical_path(tok.filename)
        if root is not None:
            self.include_resolver.set_root(root)
        elif include_resolver is None:
            self.include_resolver.set_root(None)
        self.allowed_directives: set[str]
        if allow_directives is True:
            self.allowed_directives = {"$GENERATE", "$ORIGIN", "$TTL", "$UNICODE"}
            if allow_include:
                self.allowed_directives.add("$INCLUDE")
        elif allow_directives is False:
            # allow_include was ignored in earlier releases if allow_directives was
            # False, so we continue that.
            self.allowed_directives = set()
        else:
            # Note that if directives are explicitly specified, then allow_include
            # is ignored.
            self.allowed_directives = set(_upper_dollarize(d) for d in allow_directives)
        self.force_name = force_name
        self.force_ttl = force_ttl
        self.force_rdclass = force_rdclass
        self.force_rdtype = force_rdtype
        self.txn.check_put_rdataset(_check_cname_and_other_data)

    def _eat_line(self):
        while 1:
            token = self.tok.get()
            if token.is_eol_or_eof():
                break

    def _get_identifier(self):
        token = self.tok.get()
        if not token.is_identifier():
            raise dns.exception.SyntaxError
        return token

    def _implicit_ttl(self) -> int | None:
        """Return the TTL to use for a record which did not specify one, or
        ``None`` if no TTL can be determined yet.

        An explicit default (from ``$TTL``, or from the *default_ttl* /
        *force_ttl* constructor parameters) always wins.  Otherwise, in
        ``rfc2308_ttl`` mode, the most recently stated TTL (RFC 1035 section
        5.1) is preferred over a default synthesized from the SOA MINIMUM
        field, whose overloaded "default TTL" meaning RFC 2308 section 4
        removed.  In legacy mode the SOA-derived default wins, as it did
        before RFC 2308.
        """
        if self.last_ttl_known and (  # only if the default doesn't take precedence
            not self.default_ttl_known
            or (self.rfc2308_ttl and self.default_ttl_from_soa)
        ):
            return self.last_ttl
        if self.default_ttl_known:
            return self.default_ttl
        return None

    def _rr_line(self) -> None:
        """Process one line from a DNS zone file."""
        token: dns.tokenizer.Token
        name: dns.name.Name | None
        # Name
        if self.force_name is not None:
            name = self.force_name
        else:
            if self.current_origin is None:
                raise UnknownOrigin
            token = self.tok.get(want_leading=True)
            if not token.is_whitespace():
                self.last_name = self.tok.as_name(token, self.current_origin)
            else:
                token = self.tok.get()
                if token.is_eol_or_eof():
                    # treat leading WS followed by EOL/EOF as if they were EOL/EOF.
                    return
                self.tok.unget(token)
            name = self.last_name
            if name is None:
                raise dns.exception.SyntaxError("the last used name is undefined")
            assert self.zone_origin is not None
            if not name.is_subdomain(self.zone_origin):
                self._eat_line()
                return
            if self.relativize:
                name = name.relativize(self.zone_origin)

        # TTL
        if self.force_ttl is not None:
            ttl = self.force_ttl
            self.last_ttl = ttl
            self.last_ttl_known = True
        else:
            token = self._get_identifier()
            ttl = None
            try:
                ttl = dns.ttl.from_text(token.value)
                self.last_ttl = ttl
                self.last_ttl_known = True
            except dns.ttl.BadTTL:
                self.tok.unget(token)

        # Class
        if self.force_rdclass is not None:
            rdclass = self.force_rdclass
        else:
            token = self._get_identifier()
            try:
                rdclass = dns.rdataclass.from_text(token.value)
            except dns.exception.SyntaxError:
                raise
            except Exception:
                rdclass = self.zone_rdclass
                self.tok.unget(token)
            if rdclass != self.zone_rdclass:
                raise dns.exception.SyntaxError("RR class is not zone's class")

        if ttl is None:
            # support for <class> <ttl> <type> syntax
            token = self._get_identifier()
            try:
                ttl = dns.ttl.from_text(token.value)
                self.last_ttl = ttl
                self.last_ttl_known = True
            except dns.ttl.BadTTL:
                ttl = self._implicit_ttl()
                self.tok.unget(token)

        # Type
        if self.force_rdtype is not None:
            rdtype = self.force_rdtype
        else:
            token = self._get_identifier()
            try:
                rdtype = dns.rdatatype.from_text(token.value)
            except Exception:
                raise dns.exception.SyntaxError(f"unknown rdatatype '{token.value}'")

        try:
            rd = dns.rdata.from_text(
                rdclass,
                rdtype,
                self.tok,
                self.current_origin,
                self.relativize,
                self.zone_origin,
            )
        except dns.exception.SyntaxError:
            # Catch and reraise.
            raise
        except Exception:
            # All exceptions that occur in the processing of rdata
            # are treated as syntax errors.  This is not strictly
            # correct, but it is correct almost all of the time.
            # We convert them to syntax errors so that we can emit
            # helpful filename:line info.
            ty, va = sys.exc_info()[:2]
            raise dns.exception.SyntaxError(f"caught exception {str(ty)}: {str(va)}")

        if not self.default_ttl_known and rdtype == dns.rdatatype.SOA:
            # The pre-RFC2308 and pre-BIND9 behavior inherits the zone default
            # TTL from the SOA minttl if no $TTL statement is present before the
            # SOA is parsed.
            soa_rd = cast(dns.rdtypes.ANY.SOA.SOA, rd)
            self.default_ttl = soa_rd.minimum
            self.default_ttl_known = True
            self.default_ttl_from_soa = True
            if ttl is None:
                # if we didn't have a TTL on the SOA, set it!
                ttl = self._implicit_ttl()

        # TTL check.  We had to wait until now to do this as the SOA RR's
        # own TTL can be inferred from its minimum.
        if ttl is None:
            raise dns.exception.SyntaxError("Missing default TTL value")

        self.txn.add(name, ttl, rd)

    def _generate_line(self):
        # range lhs [ttl] [class] type rhs [ comment ]
        """Process one line containing the GENERATE statement from a DNS
        zone file."""
        if self.current_origin is None:
            raise UnknownOrigin

        token = self.tok.get()
        # Range (required)
        try:
            start, stop, step = dns.grange.from_text(token.value)
            token = self.tok.get()
            if not token.is_identifier():
                raise dns.exception.SyntaxError
        except Exception:
            raise dns.exception.SyntaxError

        # lhs (required)
        try:
            lhs = token.value
            token = self.tok.get()
            if not token.is_identifier():
                raise dns.exception.SyntaxError
        except Exception:
            raise dns.exception.SyntaxError

        # TTL
        try:
            ttl = dns.ttl.from_text(token.value)
            self.last_ttl = ttl
            self.last_ttl_known = True
            token = self.tok.get()
            if not token.is_identifier():
                raise dns.exception.SyntaxError
        except dns.ttl.BadTTL:
            # We don't go to the extra "look at the SOA" level of effort for
            # $GENERATE, because the user really ought to have defined a TTL
            # somehow!
            ttl = self._implicit_ttl()
            if ttl is None:
                raise dns.exception.SyntaxError("Missing default TTL value")

        # Class
        try:
            rdclass = dns.rdataclass.from_text(token.value)
            token = self.tok.get()
            if not token.is_identifier():
                raise dns.exception.SyntaxError
        except dns.exception.SyntaxError:
            raise dns.exception.SyntaxError
        except Exception:
            rdclass = self.zone_rdclass
        if rdclass != self.zone_rdclass:
            raise dns.exception.SyntaxError("RR class is not zone's class")
        # Type
        try:
            rdtype = dns.rdatatype.from_text(token.value)
            token = self.tok.get()
            if not token.is_identifier():
                raise dns.exception.SyntaxError
        except Exception:
            raise dns.exception.SyntaxError(f"unknown rdatatype '{token.value}'")

        # rhs (required)
        rhs = token.value

        for i in range(start, stop + 1, step):
            # +1 because bind is inclusive and python is exclusive

            name = _generate_text(lhs, i)
            rdata = _generate_text(rhs, i)

            self.last_name = dns.name.from_text(
                name, self.current_origin, self.tok.idna_codec
            )
            name = self.last_name
            assert self.zone_origin is not None
            if not name.is_subdomain(self.zone_origin):
                self._eat_line()
                return
            if self.relativize:
                name = name.relativize(self.zone_origin)

            try:
                rd = dns.rdata.from_text(
                    rdclass,
                    rdtype,
                    rdata,
                    self.current_origin,
                    self.relativize,
                    self.zone_origin,
                )
            except dns.exception.SyntaxError:
                # Catch and reraise.
                raise
            except Exception:
                # All exceptions that occur in the processing of rdata
                # are treated as syntax errors.  This is not strictly
                # correct, but it is correct almost all of the time.
                # We convert them to syntax errors so that we can emit
                # helpful filename:line info.
                ty, va = sys.exc_info()[:2]
                raise dns.exception.SyntaxError(
                    f"caught exception {str(ty)}: {str(va)}"
                )

            self.txn.add(name, ttl, rd)

    def read(self) -> None:
        """Read a DNS zone file and build a zone object.

        :raises dns.zone.NoSOA: if there is no SOA RR at the zone origin.
        :raises dns.zone.NoNS: if there is no NS RRset at the zone origin.
        """

        try:
            while 1:
                token = self.tok.get(True, True)
                if token.is_eof():
                    if len(self.saved_state) > 0:
                        self.include_resolver.leave()
                        (
                            self.tok,
                            self.current_origin,
                            self.last_name,
                            self.current_file,
                            self.last_ttl,
                            self.last_ttl_known,
                            self.default_ttl,
                            self.default_ttl_known,
                            self.default_ttl_from_soa,
                        ) = self.saved_state.pop(-1)
                        continue
                    if self.current_file is not None:
                        self.current_file.close()
                        self.current_file = None
                    break
                elif token.is_eol():
                    continue
                elif token.is_comment():
                    self.tok.get_eol()
                    continue
                elif (
                    token.is_identifier()
                    and token.value[0] == "$"
                    and len(self.allowed_directives) > 0
                ):
                    # Note that we only run directive processing code if at least
                    # one directive is allowed in order to be backwards compatible
                    c = token.value.upper()
                    if c not in self.allowed_directives:
                        raise dns.exception.SyntaxError(
                            f"zone file directive '{c}' is not allowed"
                        )
                    if c == "$TTL":
                        token = self.tok.get()
                        if not token.is_identifier():
                            raise dns.exception.SyntaxError("bad $TTL")
                        self.default_ttl = dns.ttl.from_text(token.value)
                        self.default_ttl_known = True
                        self.default_ttl_from_soa = False
                        self.tok.get_eol()
                    elif c == "$ORIGIN":
                        # A relative $ORIGIN is relative to the current origin
                        # (RFC 1035 section 5.1).
                        origin = self.tok.get_name(self.current_origin)
                        if not origin.is_absolute():
                            raise UnknownOrigin
                        self.current_origin = origin
                        self.tok.get_eol()
                        if self.zone_origin is None:
                            self.zone_origin = self.current_origin
                        self.txn._set_origin(self.current_origin)
                    elif c == "$INCLUDE":
                        # Remember where the directive is, for diagnostics
                        # and as the base for resolving relative filenames.
                        include_filename, include_line = self.tok.where()
                        token = self.tok.get()
                        if not (token.is_identifier() or token.is_quoted_string()):
                            raise dns.exception.SyntaxError("bad $INCLUDE")
                        # The token value is used as written: a path is not a
                        # DNS name, so DNS escapes must not be applied to it
                        # (a Windows path such as "C:\x" would lose the
                        # backslashes).
                        filename = token.value
                        token = self.tok.get()
                        new_origin: dns.name.Name | None
                        if token.is_identifier():
                            new_origin = dns.name.from_text(
                                token.value, self.current_origin, self.tok.idna_codec
                            )
                            self.tok.get_eol()
                        elif not token.is_eol_or_eof():
                            raise dns.exception.SyntaxError("bad origin in $INCLUDE")
                        else:
                            new_origin = self.current_origin
                        origin = IncludeOrigin(
                            include_filename,
                            include_line,
                            self.include_resolver.current_path(),
                        )
                        # Open the file through the resolver, which also
                        # checks it is allowed, is not a cycle, and is not too
                        # deeply nested.
                        _, file = self.include_resolver.enter(filename, origin)
                        self.saved_state.append(
                            (
                                self.tok,
                                self.current_origin,
                                self.last_name,
                                self.current_file,
                                self.last_ttl,
                                self.last_ttl_known,
                                self.default_ttl,
                                self.default_ttl_known,
                                self.default_ttl_from_soa,
                            )
                        )
                        self.current_file = file
                        self.tok = dns.tokenizer.Tokenizer(self.current_file, filename)
                        self.current_origin = new_origin
                    elif c == "$GENERATE":
                        self._generate_line()
                    elif c == "$UNICODE":
                        while True:
                            token = self.tok.get()
                            if token.is_eol_or_eof():
                                break
                            if not token.is_identifier():
                                raise dns.exception.SyntaxError("bad $UNICODE")
                            self.txn.add_unicode(token.value)
                            if token.value == "2008":
                                self.tok.idna_codec = dns.name.IDNA_2008_Practical
                            elif token.value == "2003":
                                self.tok.idna_codec = dns.name.IDNA_2003_Practical
                    else:
                        raise dns.exception.SyntaxError(
                            f"Unknown zone file directive '{c}'"
                        )
                    continue
                self.tok.unget(token)
                self._rr_line()
        except IncludeError:
            # Include errors already carry their full include chain, so do not
            # wrap them with a location prefix as well.
            raise
        except dns.exception.SyntaxError as detail:
            filename, line_number = self.tok.where()
            if detail is None:
                detail = "syntax error"
            chain = self._include_chain()
            if chain:
                # Show the whole include chain, with the location of the
                # actual error last.
                here = f"{filename}:{line_number}"
                detail = f"{detail}\n{_format_include_chain(chain + [here])}"
            ex = dns.exception.SyntaxError(f"{filename}:{line_number}: {detail}")
            tb = sys.exc_info()[2]
            raise ex.with_traceback(tb) from None
        finally:
            # On success the include stack has been fully unwound, so this is
            # a no-op; on any failure it closes the files left open.
            self._cleanup()

    def _include_chain(self) -> list[str]:
        """Return the locations of the `$INCLUDE` directives being read.

        This is the chain leading to the file currently being read, from the
        outermost directive inwards.  It deliberately does not name the
        current location, as the caller already knows that.
        """
        return self.include_resolver.include_chain()

    def _cleanup(self) -> None:
        """Close any included files and clear the include stack."""
        if self.current_file is not None:
            self.current_file.close()
            self.current_file = None
        self.saved_state.clear()
        self.include_resolver.reset()


class RRsetsReaderTransaction(dns.transaction.Transaction):
    def __init__(self, manager, replacement, read_only):
        assert not read_only
        super().__init__(manager, replacement, read_only)
        self.rdatasets = {}

    def _get_rdataset(self, name, rdtype, covers):
        return self.rdatasets.get((name, rdtype, covers))

    def _get_node(self, name):
        rdatasets = []
        for (rdataset_name, _, _), rdataset in self.rdatasets.items():
            if name == rdataset_name:
                rdatasets.append(rdataset)
        if len(rdatasets) == 0:
            return None
        node = dns.node.Node()
        node.rdatasets = rdatasets
        return node

    def _put_rdataset(self, name, rdataset):
        self.rdatasets[(name, rdataset.rdtype, rdataset.covers)] = rdataset

    def _delete_name(self, name):
        # First remove any changes involving the name
        remove = []
        for key in self.rdatasets:
            if key[0] == name:
                remove.append(key)
        if len(remove) > 0:
            for key in remove:
                del self.rdatasets[key]

    def _delete_rdataset(self, name, rdtype, covers):
        try:
            del self.rdatasets[(name, rdtype, covers)]
        except KeyError:
            pass

    def _name_exists(self, name):
        for n, _, _ in self.rdatasets:
            if n == name:
                return True
        return False

    def _changed(self):
        return len(self.rdatasets) > 0

    def _end_transaction(self, commit):
        if commit and self._changed():
            rrsets = []
            for (name, _, _), rdataset in self.rdatasets.items():
                rrset = dns.rrset.RRset(
                    name, rdataset.rdclass, rdataset.rdtype, rdataset.covers
                )
                rrset.update(rdataset)
                rrsets.append(rrset)
            manager = cast(RRSetsReaderManager, self.manager)
            manager.set_rrsets(rrsets)

    def _set_origin(self, origin):
        pass

    def _iterate_rdatasets(self):
        raise NotImplementedError  # pragma: no cover

    def _iterate_names(self):
        raise NotImplementedError  # pragma: no cover


class RRSetsReaderManager(dns.transaction.TransactionManager):
    def __init__(
        self,
        origin: dns.name.Name | None = dns.name.root,
        relativize: bool = False,
        rdclass: dns.rdataclass.RdataClass = dns.rdataclass.IN,
    ):
        self.origin = origin
        self.relativize = relativize
        self.rdclass = rdclass
        self.rrsets: list[dns.rrset.RRset] = []

    def reader(self):  # pragma: no cover
        raise NotImplementedError

    def writer(self, replacement=False):
        assert replacement is True
        return RRsetsReaderTransaction(self, True, False)

    def get_class(self):
        return self.rdclass

    def origin_information(self):
        if self.relativize:
            effective = dns.name.empty
        else:
            effective = self.origin
        return (self.origin, self.relativize, effective)

    def set_rrsets(self, rrsets: list[dns.rrset.RRset]) -> None:
        self.rrsets = rrsets


def read_rrsets(
    text: Any,
    name: dns.name.Name | str | None = None,
    ttl: int | None = None,
    rdclass: dns.rdataclass.RdataClass | str | None = dns.rdataclass.IN,
    default_rdclass: dns.rdataclass.RdataClass | str = dns.rdataclass.IN,
    rdtype: dns.rdatatype.RdataType | str | None = None,
    default_ttl: int | str | None = None,
    idna_codec: dns.name.IDNACodec | None = None,
    origin: dns.name.Name | str | None = dns.name.root,
    relativize: bool = False,
    rfc2308_ttl: bool = True,
) -> list[dns.rrset.RRset]:
    """Read one or more rrsets from the specified text, possibly subject
    to restrictions.

    :param text: The input to process, either a file object or a string.
    :param name: The owner name of the rrset.  If not ``None``, the owner
        name is forced and the input must not specify one.  If ``None``,
        any owner names are allowed and must be present in the input.
    :type name: str, :py:class:`dns.name.Name`, or ``None``
    :param ttl: If not ``None``, the TTL is forced to the specified value
        and the input must not specify a TTL.  If ``None``, a TTL may be
        specified in the input; if not specified, *default_ttl* will be used.
    :type ttl: int, str, or ``None``
    :param rdclass: If not ``None``, the class is forced to the specified
        value and the input must not specify a class.  If ``None``, the input
        may optionally specify a class matching *default_rdclass*.  Note that
        rrsets with differing classes cannot be returned; ``None`` here simply
        allows the user to optionally write a class in the input.
    :type rdclass: :py:class:`dns.rdataclass.RdataClass`, str, or ``None``
    :param default_rdclass: The class of the returned rrsets.
    :type default_rdclass: :py:class:`dns.rdataclass.RdataClass` or str
    :param rdtype: If not ``None``, the type is forced to the specified value
        and the input must not specify a type.  If ``None``, a type must be
        present for each RR.
    :type rdtype: :py:class:`dns.rdatatype.RdataType`, str, or ``None``
    :param default_ttl: If not ``None``, and the TTL is not forced and not
        specified in the input, this value will be used.  If ``None``, an
        error will occur if the TTL is not forced and not specified.
    :type default_ttl: int, str, or ``None``
    :param idna_codec: The IDNA encoder/decoder.  If ``None``, the default
        IDNA encoder/decoder is used.  Note that codecs only apply to the
        owner name; dnspython does not do IDNA for names in rdata.
    :type idna_codec: :py:class:`dns.name.IDNACodec` or ``None``
    :param origin: The origin for any relative names in the input, and also
        the origin to relativize to if *relativize* is ``True``.
    :type origin: str, :py:class:`dns.name.Name`, or ``None``
    :param bool relativize: If ``True``, names are relativized to *origin*;
        if ``False``, relative names in the input are made absolute by
        appending *origin*.
    :param bool rfc2308_ttl: If ``True``, a TTL-less record inherits the most
        recently stated TTL in preference to a default derived from the SOA
        MINIMUM field, per RFC 2308 section 4.  The default is ``True``.  If
        ``False``, the historical pre-RFC 2308 behavior is used.
    :rtype: list[:py:class:`dns.rrset.RRset`]
    """
    if isinstance(origin, str):
        origin = dns.name.from_text(origin, dns.name.root, idna_codec)
    if isinstance(name, str):
        name = dns.name.from_text(name, origin, idna_codec)
    if isinstance(ttl, str):
        ttl = dns.ttl.from_text(ttl)
    if isinstance(default_ttl, str):
        default_ttl = dns.ttl.from_text(default_ttl)
    if rdclass is not None:
        rdclass = dns.rdataclass.RdataClass.make(rdclass)
    else:
        rdclass = None
    default_rdclass = dns.rdataclass.RdataClass.make(default_rdclass)
    if rdtype is not None:
        rdtype = dns.rdatatype.RdataType.make(rdtype)
    else:
        rdtype = None
    manager = RRSetsReaderManager(origin, relativize, default_rdclass)
    with manager.writer(True) as txn:
        tok = dns.tokenizer.Tokenizer(text, "<input>", idna_codec=idna_codec)
        reader = Reader(
            tok,
            default_rdclass,
            txn,
            allow_directives=False,
            force_name=name,
            force_ttl=ttl,
            force_rdclass=rdclass,
            force_rdtype=rdtype,
            default_ttl=default_ttl,
            rfc2308_ttl=rfc2308_ttl,
        )
        reader.read()
    return manager.rrsets
