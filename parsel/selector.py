"""XPath and JMESPath selectors based on the lxml and jmespath Python
packages."""

from __future__ import annotations

import codecs
import json
import re
import typing
import warnings
from dataclasses import dataclass, replace
from functools import lru_cache
from io import BytesIO
from typing import (
    TYPE_CHECKING,
    Any,
    Literal,
    Self,
    SupportsIndex,
    TypeAlias,
    TypedDict,
    TypeVar,
)

import jmespath
from lxml import etree, html

from .csstranslator import GenericTranslator, HTMLTranslator
from .utils import extract_regex, flatten, iflatten, shorten

if TYPE_CHECKING:
    from collections.abc import Mapping
    from re import Pattern


_SelectorType = TypeVar("_SelectorType", bound="Selector")
_ParserType: TypeAlias = etree.XMLParser | etree.HTMLParser
# simplified _OutputMethodArg from types-lxml
_TostringMethodType = Literal[
    "html",
    "xml",
]


class CannotRemoveElementWithoutRoot(Exception):
    pass


class CannotRemoveElementWithoutParent(Exception):
    pass


class CannotDropElementWithoutParent(CannotRemoveElementWithoutParent):
    pass


class AbsoluteXPathWarning(UserWarning):
    """Emitted when an absolute XPath is used on a nested selector."""


#: Kinds of results a :class:`Provenance` can describe.
ProvenanceKind: TypeAlias = Literal["element", "attribute", "text", "json", "unknown"]


@dataclass(frozen=True, eq=False)
class Provenance:
    """Provenance of a :class:`Selector` result.

    A provenance record is an immutable snapshot taken when the query that
    produced the selector was evaluated. It stores lightweight coordinates
    (a line number, a node path and the chain of queries), never a copy of
    the document, and it is never updated afterwards: dropping or moving
    nodes in the underlying tree does not change it, nor does it silently
    start pointing at different nodes.

    ``line`` is the 1-based source line of the node in the parsed document,
    or ``None`` when it is unknown.

    ``path`` is the path of the node within its document (as returned by
    :meth:`lxml.etree._ElementTree.getpath`), or ``None`` when it is unknown.

    ``query_chain`` is the tuple of queries (XPath, CSS, JMESPath or
    JSONPath) that were applied, in order, to reach this result from the
    root selector.

    ``kind`` is ``"element"`` for node results, ``"attribute"`` or
    ``"text"`` for attribute and text results (whose ``line`` and ``path``
    point to the element that owns them), ``"json"`` for JSON results and
    ``"unknown"`` otherwise.

    ``attribute`` is the attribute name when ``kind`` is ``"attribute"``,
    ``None`` otherwise.

    ``unknown_reason`` explains why the coordinates are unknown, e.g.
    because the selector was built from a manually provided root or because
    the input is JSON, which has no source coordinates. It is ``None`` when
    the coordinates are known.

    Two provenance records are never considered equal, even if all their
    fields match: identical values coming from different nodes must not be
    merged.
    """

    line: int | None = None
    path: str | None = None
    query_chain: tuple[str, ...] = ()
    kind: ProvenanceKind = "unknown"
    attribute: str | None = None
    unknown_reason: str | None = None

    @property
    def is_unknown(self) -> bool:
        """Whether the source coordinates of this result are unknown."""
        return self.line is None and self.path is None

    def _advance(self, query: str) -> Provenance:
        """Return a copy of this record with *query* appended to the chain."""
        return replace(self, query_chain=self.query_chain + (query,))


class SafeXMLParser(etree.XMLParser):
    def __init__(self, *args: Any, **kwargs: Any) -> None:
        kwargs.setdefault("resolve_entities", False)
        super().__init__(*args, **kwargs)


class CTGroupValue(TypedDict):
    _parser: type[etree.XMLParser | html.HTMLParser]
    _csstranslator: GenericTranslator | HTMLTranslator
    _tostring_method: _TostringMethodType


_ctgroup: dict[str, CTGroupValue] = {
    "html": {
        "_parser": html.HTMLParser,
        "_csstranslator": HTMLTranslator(),
        "_tostring_method": "html",
    },
    "xml": {
        "_parser": SafeXMLParser,
        "_csstranslator": GenericTranslator(),
        "_tostring_method": "xml",
    },
}


def _xml_or_html(type_: str | None) -> str:
    return "xml" if type_ == "xml" else "html"


_ENCODING_ERROR_TYPES = frozenset(
    {etree.ErrorTypes.ERR_INVALID_CHAR, etree.ErrorTypes.ERR_INVALID_ENCODING}
)


def _has_encoding_error(parser: _ParserType) -> bool:
    return any(error.type in _ENCODING_ERROR_TYPES for error in parser.error_log)


def _root_from_decoded_body(
    body: bytes,
    parser_cls: type[_ParserType],
    base_url: str | None,
    huge_tree: bool,
    encoding: str,
) -> etree._Element:
    """Create a root node from *body*, with undecodable bytes replaced."""
    return create_root_node(
        body.decode(encoding, errors="replace"),
        parser_cls,
        base_url=base_url,
        huge_tree=huge_tree,
    )


_XML_DECLARATION = re.compile(r"[\s\ufeff]*<\?xml\s")


def _detect_xml_or_html(text: str) -> str:
    return "xml" if _XML_DECLARATION.match(text) else "html"


def create_root_node(
    text: str,
    parser_cls: type[_ParserType],
    base_url: str | None = None,
    huge_tree: bool = True,
    body: bytes = b"",
    encoding: str = "utf-8",
) -> etree._Element:
    """Create root node for text using given parser class."""
    if text:
        body = text.strip().replace("\x00", "").encode(encoding) or b"<html/>"
    elif codecs.lookup(encoding).name != "utf-8":
        # lxml handles bytes that the declared encoding cannot decode by
        # truncating the document there, and characters that span several
        # bytes, as in UTF-16, are broken by the null and whitespace stripping
        # below. Only UTF-8 bytes are passed as is, since lxml does report
        # invalid UTF-8, and it covers most input.
        return _root_from_decoded_body(body, parser_cls, base_url, huge_tree, encoding)
    else:
        body = body.replace(b"\x00", b"").strip() or b"<html/>"

    parser = parser_cls(recover=True, encoding=encoding, huge_tree=huge_tree)
    root = None
    try:
        root = etree.fromstring(body, parser=parser, base_url=base_url)
    except etree.XMLSyntaxError:
        if text or not _has_encoding_error(parser):
            raise
    if not text and _has_encoding_error(parser):
        # Invalid bytes reach the tree as is, and reading them raises
        # UnicodeDecodeError from lxml.
        return _root_from_decoded_body(body, parser_cls, base_url, huge_tree, encoding)
    if not huge_tree:
        for error in parser.error_log:
            if "use XML_PARSE_HUGE option" in error.message:
                warnings.warn(
                    "Input data is too big. Set huge_tree=True for huge_tree support.",
                    stacklevel=2,
                )
    if root is None:
        root = etree.fromstring(b"<html/>", parser=parser, base_url=base_url)
    return root


class SelectorList(list[_SelectorType]):
    """
    The :class:`SelectorList` class is a subclass of the builtin ``list``
    class, which provides a few additional methods.
    """

    @typing.overload
    def __getitem__(self, pos: SupportsIndex) -> _SelectorType:
        pass

    @typing.overload
    def __getitem__(self, pos: slice) -> SelectorList[_SelectorType]:
        pass

    def __getitem__(
        self, pos: SupportsIndex | slice
    ) -> _SelectorType | SelectorList[_SelectorType]:
        o = super().__getitem__(pos)
        if isinstance(pos, slice):
            return self.__class__(typing.cast("SelectorList[_SelectorType]", o))
        return typing.cast("_SelectorType", o)

    def __getstate__(self) -> None:
        raise TypeError("can't pickle SelectorList objects")

    def jmespath(self, query: str, **kwargs: Any) -> SelectorList[_SelectorType]:
        """
        Call the ``.jmespath()`` method for each element in this list and return
        their results flattened as another :class:`SelectorList`.

        ``query`` is the same argument as the one in :meth:`Selector.jmespath`.

        Any additional named arguments are passed to the underlying
        ``jmespath.search`` call, e.g.::

            selector.jmespath('author.name', options=jmespath.Options(dict_cls=collections.OrderedDict))
        """
        return self.__class__(flatten([x.jmespath(query, **kwargs) for x in self]))

    def jsonpath(self, query: str) -> SelectorList[_SelectorType]:
        """
        Call the ``.jsonpath()`` method for each element in this list and return
        their results flattened as another :class:`SelectorList`.

        .. versionadded:: VERSION

        ``query`` is the same argument as the one in :meth:`Selector.jsonpath`.
        """
        return self.__class__(flatten([x.jsonpath(query) for x in self]))

    def xpath(
        self,
        xpath: str,
        namespaces: Mapping[str, str] | None = None,
        **kwargs: Any,
    ) -> SelectorList[_SelectorType]:
        """
        Call the ``.xpath()`` method for each element in this list and return
        their results flattened as another :class:`SelectorList`.

        ``xpath`` is the same argument as the one in :meth:`Selector.xpath`

        ``namespaces`` is an optional ``prefix: namespace-uri`` mapping (dict)
        for additional prefixes to those registered with ``register_namespace(prefix, uri)``.
        Contrary to ``register_namespace()``, these prefixes are not
        saved for future calls.

        Any additional named arguments can be used to pass values for XPath
        variables in the XPath expression, e.g.::

            selector.xpath('//a[href=$url]', url="http://www.example.com")
        """
        if xpath[:1] == "/":
            for sel in self:
                if sel._warn_if_nested(xpath, stacklevel=3):
                    break
        return self.__class__(
            flatten([x._xpath(xpath, namespaces, kwargs) for x in self])
        )

    def css(self, query: str) -> SelectorList[_SelectorType]:
        """
        Call the ``.css()`` method for each element in this list and return
        their results flattened as another :class:`SelectorList`.

        ``query`` is the same argument as the one in :meth:`Selector.css`
        """
        return self.__class__(flatten([x.css(query) for x in self]))

    def re(self, regex: str | Pattern[str], replace_entities: bool = True) -> list[str]:
        """
        Call the ``.re()`` method for each element in this list and return
        their results flattened, as a list of strings.

        By default, character entity references are replaced by their
        corresponding character (except for ``&amp;`` and ``&lt;``).
        Passing ``replace_entities`` as ``False`` switches off these
        replacements.
        """
        return flatten([x.re(regex, replace_entities=replace_entities) for x in self])

    @typing.overload
    def re_first(
        self,
        regex: str | Pattern[str],
        default: None = None,
        replace_entities: bool = True,
    ) -> str | None:
        pass

    @typing.overload
    def re_first(
        self,
        regex: str | Pattern[str],
        default: str,
        replace_entities: bool = True,
    ) -> str:
        pass

    def re_first(
        self,
        regex: str | Pattern[str],
        default: str | None = None,
        replace_entities: bool = True,
    ) -> str | None:
        """
        Call the ``.re()`` method for elements in this list and return the
        first matching result as a string. If the list is empty or the
        regex doesn't match anything, return the default value (``None`` if
        the argument is not provided).

        By default, character entity references are replaced by their
        corresponding character (except for ``&amp;`` and ``&lt;``).
        Passing ``replace_entities`` as ``False`` switches off these
        replacements.
        """
        for el in iflatten(
            x.re(regex, replace_entities=replace_entities) for x in self
        ):
            return typing.cast("str", el)
        return default

    def getall(self) -> list[str]:
        """
        Call the ``.get()`` method for each element in this list and return
        their results flattened, as a list of strings.
        """
        return [x.get() for x in self]

    extract = getall

    @typing.overload
    def get(self, default: None = None) -> str | None:
        pass

    @typing.overload
    def get(self, default: str) -> str:
        pass

    def get(self, default: str | None = None) -> Any:
        """
        Return the result of ``.get()`` for the first element in this list.
        If the list is empty, return the default value.
        """
        for x in self:
            return x.get()
        return default

    extract_first = get

    @property
    def attrib(self) -> Mapping[str, str]:
        """Return the attributes dictionary for the first element.
        If the list is empty, return an empty dict.
        """
        for x in self:
            return x.attrib
        return {}

    def drop(self) -> None:
        """
        Drop matched nodes from the parent for each element in this list.
        """
        for x in self:
            x.drop()


_NOT_SET = object()


def _get_root_from_text(text: str, *, type_: str, **lxml_kwargs: Any) -> etree._Element:
    return create_root_node(text, _ctgroup[type_]["_parser"], **lxml_kwargs)


def _get_root_and_type_from_bytes(
    body: bytes,
    encoding: str,
    *,
    input_type: str | None,
    **lxml_kwargs: Any,
) -> tuple[Any, str]:
    if input_type == "text":
        return body.decode(encoding), input_type
    if input_type in ("json", None) and codecs.lookup(encoding).name == "utf-8":
        try:
            data = json.load(BytesIO(body))
        except ValueError:
            data = _NOT_SET
        if data is not _NOT_SET and (input_type == "json" or _is_json_document(data)):
            return data, "json"
    if input_type == "json":
        return None, "json"
    assert input_type in ("html", "xml", None)  # nosec
    type_ = input_type or _detect_xml_or_html(body[:256].decode(encoding, "ignore"))
    root = create_root_node(
        text="",
        body=body,
        encoding=encoding,
        parser_cls=_ctgroup[type_]["_parser"],
        **lxml_kwargs,
    )
    return root, type_


def _get_root_and_type_from_text(
    text: str, *, input_type: str | None, **lxml_kwargs: Any
) -> tuple[Any, str]:
    if input_type == "text":
        return text, input_type
    if input_type in ("json", None):
        try:
            data = json.loads(text)
        except ValueError:
            data = _NOT_SET
        if data is not _NOT_SET and (input_type == "json" or _is_json_document(data)):
            return data, "json"
    if input_type == "json":
        return None, "json"
    assert input_type in ("html", "xml", None)  # nosec
    type_ = input_type or _detect_xml_or_html(text)
    root = _get_root_from_text(text, type_=type_, **lxml_kwargs)
    return root, type_


# lxml registers the functions of these namespaces on every call of an
# evaluator that maps them, and with libxml2 2.13+ every registration after the
# first adds an entry to the evaluator's error log, which cannot be cleared.
_EXSLT_NAMESPACES = frozenset(
    f"http://exslt.org/{name}"
    for name in ("dates-and-times", "math", "sets", "strings")
)


@lru_cache(maxsize=2048)
def _compile_xpath(
    query: str, namespaces: tuple[tuple[str, str], ...], smart_strings: bool
) -> etree.XPath:
    return etree.XPath(query, namespaces=dict(namespaces), smart_strings=smart_strings)


def _get_xpath_evaluator(
    query: str, namespaces: dict[str, str], smart_strings: bool
) -> etree.XPath:
    namespaces = {
        prefix: uri
        for prefix, uri in namespaces.items()
        if uri not in _EXSLT_NAMESPACES
        or not isinstance(query, str)
        or f"{prefix}:" in query
    }
    if _EXSLT_NAMESPACES.intersection(namespaces.values()):
        return etree.XPath(query, namespaces=namespaces, smart_strings=smart_strings)
    return _compile_xpath(query, tuple(sorted(namespaces.items())), smart_strings)


def _get_root_type(root: Any, *, input_type: str | None) -> str:
    if isinstance(root, etree._Element):
        if input_type in {"json", "text"}:
            raise ValueError(
                f"Selector got an lxml.etree._Element object as root, "
                f"and {input_type!r} as type."
            )
        return _xml_or_html(input_type)
    return input_type or "json"


def _is_json_document(data: Any) -> bool:
    """Return whether *data* is a JSON object or array.

    Scalar JSON values are valid JSON documents as well, but they are also
    valid text and HTML, which is what they usually are when type detection
    gets to see them, so type detection ignores them.
    """
    return isinstance(data, (dict, list))


def _load_json_or_none(text: str | None) -> Any:
    try:
        return json.loads(text)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return None


class JSONPathError(ValueError):
    """Raised when a JSONPath query is invalid or cannot be evaluated.

    .. versionadded:: VERSION
    """


@lru_cache(maxsize=1)
def _jsonpath_backend() -> tuple[Any, Any, type[Exception]]:
    """Return a query compiler, a function that runs a compiled query on data,
    and the base error class of the underlying JSONPath package."""
    try:
        import jsonpath_rfc9535  # noqa: PLC0415
    except ImportError:
        pass
    else:
        rfc9535_env = jsonpath_rfc9535.JSONPathEnvironment()
        return (
            rfc9535_env.compile,
            lambda compiled, data: compiled.find(data).values(),
            jsonpath_rfc9535.JSONPathError,
        )
    try:
        import jsonpath  # noqa: PLC0415
    except ImportError:
        raise ImportError(
            "You need to install jsonpath-rfc9535 or python-jsonpath, e.g. "
            "pip install parsel[jsonpath-rfc9535], to use JSONPath "
            "expressions."
        )
    # strict mode restricts the syntax to RFC 9535, leaving out the extensions
    # that python-jsonpath enables by default.
    jsonpath_env = jsonpath.JSONPathEnvironment(strict=True)
    return (
        jsonpath_env.compile,
        lambda compiled, data: compiled.findall(data),
        jsonpath.JSONPathError,
    )


@lru_cache(maxsize=512)
def _compile_jsonpath(query: str) -> Any:
    compile_query, run_query, backend_error = _jsonpath_backend()
    try:
        compiled = compile_query(query)
    except backend_error as error:
        raise JSONPathError(str(error))

    def find(data: Any) -> Any:
        try:
            return run_query(compiled, data)
        except backend_error as error:
            raise JSONPathError(str(error))

    return find


class Selector:
    """Wrapper for input data in HTML, JSON, or XML format, that allows
    selecting parts of it using selection expressions.

    You can write selection expressions in CSS or XPath for HTML and XML
    inputs, or in JMESPath or JSONPath for JSON inputs.

    ``text`` is an ``str`` object.

    ``body`` is a ``bytes`` or ``bytearray`` object. It can be used together with the
    ``encoding`` argument instead of the ``text`` argument.

    ``type`` defines the selector type. It can be ``"html"``, ``"json"``,
    ``"xml"`` or ``"text"``. If not specified, the input is handled as
    ``"json"`` if it is a JSON object or array, as ``"xml"`` if it starts with
    an XML declaration, and as ``"html"`` otherwise.

    ``base_url`` allows setting a URL for the document. This is needed when looking up external entities with relative paths.
    See the documentation for :func:`lxml.etree.fromstring` for more information.

    ``huge_tree`` controls the lxml/libxml2 feature that forbids parsing
    certain large documents to protect from possible memory exhaustion. The
    argument is ``True`` by default, which disables the protection to allow
    parsing such documents. Set it to ``False`` if you want to enable the
    protection. See `this lxml FAQ entry
    <https://lxml.de/FAQ.html#is-lxml-vulnerable-to-xml-bombs>`_ for more
    information.

    ``provenance`` enables provenance tracking. When it is ``True``, every
    selector, including the results of further queries, gets a
    :attr:`provenance` record with the source line, the node path within the
    document and the chain of queries that produced it. Provenance tracking
    does not change the return values of :meth:`get` or :meth:`getall`.

    .. versionadded:: VERSION
       The ``provenance`` argument and the :attr:`provenance` attribute.
    """

    __slots__ = [
        "__weakref__",
        "_expr",
        "_huge_tree",
        "_provenance",
        "_text",
        "_track_provenance",
        "body",
        "namespaces",
        "root",
        "type",
    ]

    _default_namespaces = {
        "re": "http://exslt.org/regular-expressions",
        # supported in libxslt:
        # set:difference
        # set:has-same-node
        # set:intersection
        # set:leading
        # set:trailing
        "set": "http://exslt.org/sets",
    }
    _lxml_smart_strings = False
    selectorlist_cls = SelectorList["Selector"]

    def __init__(
        self,
        text: str | None = None,
        type: str | None = None,  # noqa: A002
        body: bytes | bytearray = b"",
        encoding: str = "utf-8",
        namespaces: Mapping[str, str] | None = None,
        root: Any | None = _NOT_SET,
        base_url: str | None = None,
        _expr: str | None = None,
        huge_tree: bool = True,
        provenance: bool = False,
        _provenance: Provenance | None = None,
    ) -> None:
        self.root: Any
        if type not in ("html", "json", "text", "xml", None):
            raise ValueError(f"Invalid type: {type}")

        parsed = False
        if text is not None:
            if root is not _NOT_SET:
                warnings.warn(
                    "Selector got both text and root, root is being ignored.",
                    stacklevel=2,
                )
            if not isinstance(text, str):
                msg = f"text argument should be of type str, got {text.__class__}"
                raise TypeError(msg)

            root, type = _get_root_and_type_from_text(  # noqa: A001
                text,
                input_type=type,
                base_url=base_url,
                huge_tree=huge_tree,
            )
            self.root = root
            self.type = type
            parsed = True
        elif body:
            if not isinstance(body, (bytes, bytearray)):
                msg = f"body argument should be of type bytes or bytearray, got {body.__class__}"
                raise TypeError(msg)
            root, type = _get_root_and_type_from_bytes(  # noqa: A001
                body=bytes(body),
                encoding=encoding,
                input_type=type,
                base_url=base_url,
                huge_tree=huge_tree,
            )
            self.root = root
            self.type = type
            parsed = True
        elif root is _NOT_SET:
            raise ValueError("Selector needs text, body, or root arguments")
        else:
            self.root = root
            self.type = _get_root_type(root, input_type=type)

        self.namespaces = dict(self._default_namespaces)
        if namespaces is not None:
            self.namespaces.update(namespaces)

        self._expr = _expr
        self._huge_tree = huge_tree
        self._text = text
        self._track_provenance = provenance
        if provenance:
            self._provenance = (
                _provenance if _provenance is not None else self._root_provenance(parsed)
            )
        else:
            self._provenance = None

    def __getstate__(self) -> Any:
        raise TypeError("can't pickle Selector objects")

    @property
    def provenance(self) -> Provenance | None:
        """Provenance of this selector, as a :class:`Provenance` record.

        It is ``None`` unless the root selector was created with
        ``provenance=True``.

        The record is a snapshot taken when the query that produced this
        selector was evaluated. Later changes to the underlying document,
        such as dropping nodes with :meth:`drop`, do not update it.

        .. versionadded:: VERSION
        """
        return self._provenance

    def _provenance_kwargs(self, provenance: Provenance | None) -> dict[str, Any]:
        """Return the extra constructor arguments for a child selector.

        They are only passed when provenance tracking is enabled, so that
        subclasses with a custom ``__init__`` that predates provenance
        support keep working as long as the feature is off.
        """
        if not self._track_provenance:
            return {}
        return {"provenance": True, "_provenance": provenance}

    def _root_provenance(self, parsed: bool) -> Provenance:
        """Build the provenance record for a root selector."""
        chain = (self._expr,) if self._expr else ()
        if parsed and isinstance(self.root, etree._Element):
            return Provenance(
                line=self.root.sourceline,
                path=self.root.getroottree().getpath(self.root),
                query_chain=chain,
                kind="element",
            )
        if self.type == "json":
            return Provenance(
                query_chain=chain,
                kind="json",
                unknown_reason="JSON data has no source coordinates",
            )
        if not isinstance(self.root, etree._Element):
            return Provenance(
                query_chain=chain,
                kind="unknown",
                unknown_reason=f"{self.type!r} input has no source coordinates",
            )
        return Provenance(
            query_chain=chain,
            kind="unknown",
            unknown_reason="the selector was built from a manually provided root",
        )

    def _query_chain(self, query: str) -> tuple[str, ...]:
        if self._provenance is None:
            return (query,)
        return self._provenance.query_chain + (query,)

    def _node_provenance(self, node: Any, query: str) -> Provenance | None:
        """Build the provenance record for an XPath query result."""
        if not self._track_provenance:
            return None
        chain = self._query_chain(query)
        if isinstance(node, etree._Element):
            return Provenance(
                line=node.sourceline,
                path=node.getroottree().getpath(node),
                query_chain=chain,
                kind="element",
            )
        if isinstance(node, etree._ElementUnicodeResult):
            parent = node.getparent()
            if parent is None:
                return Provenance(
                    query_chain=chain,
                    kind="unknown",
                    unknown_reason="the result is not linked to a parent element",
                )
            if node.is_attribute:
                return Provenance(
                    line=parent.sourceline,
                    path=parent.getroottree().getpath(parent),
                    query_chain=chain,
                    kind="attribute",
                    attribute=node.attrname,
                )
            return Provenance(
                line=parent.sourceline,
                path=parent.getroottree().getpath(parent),
                query_chain=chain,
                kind="text",
            )
        return Provenance(
            query_chain=chain,
            kind="unknown",
            unknown_reason="the query result is not a document node",
        )

    def _json_provenance(self, query: str) -> Provenance | None:
        """Build the provenance record for a JMESPath or JSONPath result."""
        if not self._track_provenance:
            return None
        return Provenance(
            query_chain=self._query_chain(query),
            kind="json",
            unknown_reason="JSON data has no source coordinates",
        )

    def _get_root(
        self,
        text: str = "",
        base_url: str | None = None,
        huge_tree: bool = True,
        type_: str | None = None,
        body: bytes = b"",
        encoding: str = "utf-8",
    ) -> etree._Element:
        return create_root_node(
            text,
            body=body,
            encoding=encoding,
            parser_cls=_ctgroup[type_ or self.type]["_parser"],
            base_url=base_url,
            huge_tree=huge_tree,
        )

    def _json_data(self) -> Any:
        if isinstance(self.root, str):
            return _load_json_or_none(self.root)
        if self.type == "json":
            return self.root
        assert self.type in {"html", "xml"}  # nosec
        return _load_json_or_none(self.root.text)

    def _json_selector_list(self, values: Any, query: str) -> SelectorList[Self]:
        def make_selector(x: Any) -> Selector:  # closure function
            kwargs = self._provenance_kwargs(self._json_provenance(query))
            if isinstance(x, str):
                return self.__class__(text=x, _expr=query, type="text", **kwargs)
            return self.__class__(root=x, _expr=query, **kwargs)

        return typing.cast(
            "SelectorList[Self]",
            self.selectorlist_cls([make_selector(x) for x in values]),
        )

    def jmespath(
        self,
        query: str,
        **kwargs: Any,
    ) -> SelectorList[Self]:
        """
        Find objects matching the JMESPath ``query`` and return the result as a
        :class:`SelectorList` instance with all elements flattened. List
        elements implement :class:`Selector` interface too.

        ``query`` is a string containing the `JMESPath
        <https://jmespath.org/>`_ query to apply.

        Any additional named arguments are passed to the underlying
        ``jmespath.search`` call, e.g.::

            selector.jmespath('author.name', options=jmespath.Options(dict_cls=collections.OrderedDict))
        """
        result = jmespath.search(query, self._json_data(), **kwargs)
        if result is None:
            result = []
        elif not isinstance(result, list):
            result = [result]
        return self._json_selector_list(result, query)

    def jsonpath(self, query: str) -> SelectorList[Self]:
        """
        Find objects matching the JSONPath ``query`` and return the result as a
        :class:`SelectorList` instance with all elements flattened. List
        elements implement :class:`Selector` interface too.

        .. versionadded:: VERSION

        ``query`` is a string containing the `JSONPath
        <https://datatracker.ietf.org/doc/html/rfc9535>`_ query to apply.
        Invalid queries raise :class:`JSONPathError`.

        Requires a JSONPath package, see :ref:`jsonpath-install`.
        """
        return self._json_selector_list(
            _compile_jsonpath(query)(self._json_data()), query
        )

    def xpath(
        self,
        query: str,
        namespaces: Mapping[str, str] | None = None,
        **kwargs: Any,
    ) -> SelectorList[Self]:
        """
        Find nodes matching the xpath ``query`` and return the result as a
        :class:`SelectorList` instance with all elements flattened. List
        elements implement :class:`Selector` interface too.

        ``query`` is a string containing the XPATH query to apply.

        ``namespaces`` is an optional ``prefix: namespace-uri`` mapping (dict)
        for additional prefixes to those registered with ``register_namespace(prefix, uri)``.
        Contrary to ``register_namespace()``, these prefixes are not
        saved for future calls.

        Any additional named arguments can be used to pass values for XPath
        variables in the XPath expression, e.g.::

            selector.xpath('//a[href=$url]', url="http://www.example.com")
        """
        if query[:1] == "/":
            self._warn_if_nested(query, stacklevel=3)
        return self._xpath(query, namespaces, kwargs)

    def _warn_if_nested(self, xpath: str, *, stacklevel: int) -> bool:
        if isinstance(self.root, etree._Element) and self.root.getparent() is not None:
            warnings.warn(
                f"Absolute XPath {xpath!r} used on a nested selector",
                AbsoluteXPathWarning,
                stacklevel=stacklevel,
            )
            return True
        return False

    def _xpath(
        self,
        query: str,
        namespaces: Mapping[str, str] | None,
        kwargs: dict[str, Any],
        _chain_query: str | None = None,
    ) -> SelectorList[Self]:
        if self.type not in ("html", "xml", "text"):
            raise ValueError(f"Cannot use xpath on a Selector of type {self.type!r}")
        chain_query = query if _chain_query is None else _chain_query
        if self.type in ("html", "xml"):
            root = self.root
            if not hasattr(root, "xpath"):
                if isinstance(root, str) and query.strip() == ".":
                    provenance = (
                        self._provenance._advance(chain_query)
                        if self._provenance is not None
                        else None
                    )
                    return typing.cast(
                        "SelectorList[Self]",
                        self.selectorlist_cls(
                            [
                                self.__class__(
                                    root=self.root,
                                    _expr=query,
                                    namespaces=self.namespaces,
                                    type=self.type,
                                    **self._provenance_kwargs(provenance),
                                )
                            ]
                        ),
                    )
                return typing.cast("SelectorList[Self]", self.selectorlist_cls([]))
        else:
            root = self._get_root(self._text or "", type_="html")

        nsp = dict(self.namespaces)
        if namespaces is not None:
            nsp.update(namespaces)
        try:
            # Smart strings are needed in provenance mode so that text and
            # attribute results can be traced back to their owning element.
            smart_strings = self._lxml_smart_strings or self._track_provenance
            xpathev = _get_xpath_evaluator(query, nsp, smart_strings)
            result = xpathev(root, **kwargs)
        except etree.XPathError as exc:
            raise ValueError(f"XPath error: {exc} in {query}")

        if not isinstance(result, list):
            result = [result]

        result = [
            self.__class__(
                root=str(x)
                if self._track_provenance
                and isinstance(x, etree._ElementUnicodeResult)
                else x,
                _expr=query,
                namespaces=self.namespaces,
                type=_xml_or_html(self.type),
                **self._provenance_kwargs(self._node_provenance(x, chain_query)),
            )
            for x in result
        ]
        return typing.cast("SelectorList[Self]", self.selectorlist_cls(result))

    def css(self, query: str) -> SelectorList[Self]:
        """
        Apply the given CSS selector and return a :class:`SelectorList` instance.

        ``query`` is a string containing the CSS selector to apply.

        In the background, CSS queries are translated into XPath queries using
        `cssselect`_ library and run ``.xpath()`` method.

        .. _cssselect: https://pypi.python.org/pypi/cssselect/
        """
        if self.type not in ("html", "xml", "text"):
            raise ValueError(f"Cannot use css on a Selector of type {self.type!r}")
        # _chain_query keeps the CSS query itself in the provenance query
        # chain instead of the XPath it is translated into.
        return self._xpath(self._css2xpath(query), None, {}, _chain_query=query)

    def _css2xpath(self, query: str) -> str:
        type_ = _xml_or_html(self.type)
        return _ctgroup[type_]["_csstranslator"].css_to_xpath(query)

    def re(self, regex: str | Pattern[str], replace_entities: bool = True) -> list[str]:
        """
        Apply the given regex and return a list of strings with the
        matches.

        ``regex`` can be either a compiled regular expression or a string which
        will be compiled to a regular expression using ``re.compile(regex)``.

        By default, character entity references are replaced by their
        corresponding character (except for ``&amp;`` and ``&lt;``).
        Passing ``replace_entities`` as ``False`` switches off these
        replacements.
        """
        data = self.get()
        return extract_regex(regex, data, replace_entities=replace_entities)

    @typing.overload
    def re_first(
        self,
        regex: str | Pattern[str],
        default: None = None,
        replace_entities: bool = True,
    ) -> str | None:
        pass

    @typing.overload
    def re_first(
        self,
        regex: str | Pattern[str],
        default: str,
        replace_entities: bool = True,
    ) -> str:
        pass

    def re_first(
        self,
        regex: str | Pattern[str],
        default: str | None = None,
        replace_entities: bool = True,
    ) -> str | None:
        """
        Apply the given regex and return the first string which matches. If
        there is no match, return the default value (``None`` if the argument
        is not provided).

        By default, character entity references are replaced by their
        corresponding character (except for ``&amp;`` and ``&lt;``).
        Passing ``replace_entities`` as ``False`` switches off these
        replacements.
        """
        return next(
            iflatten(self.re(regex, replace_entities=replace_entities)),
            default,
        )

    def get(self) -> Any:
        """
        Serialize and return the matched nodes.

        For HTML and XML, the result is always a string.
        """
        if self.type in ("text", "json"):
            return self.root
        try:
            return etree.tostring(
                self.root,
                method=_ctgroup[self.type]["_tostring_method"],
                encoding="unicode",
                with_tail=False,
            )
        except (AttributeError, TypeError):
            if self.root is True:
                return "1"
            if self.root is False:
                return "0"
            return str(self.root)

    extract = get

    def getall(self) -> list[str]:
        """
        Serialize and return the matched node in a 1-element list of strings.
        """
        return [self.get()]

    def register_namespace(self, prefix: str, uri: str) -> None:
        """
        Register the given namespace to be used in this :class:`Selector`.
        Without registering namespaces you can't select or extract data from
        non-standard namespaces. See :ref:`selector-examples-xml`.
        """
        self.namespaces[prefix] = uri

    def remove_namespaces(self) -> None:
        """
        Remove all namespaces, allowing to traverse the document using
        namespace-less xpaths. See :ref:`removing-namespaces`.
        For JSON selectors, this method does nothing.
        """
        if self.type == "json":
            return

        for el in self.root.iter("*"):
            if el.tag.startswith("{"):
                el.tag = el.tag.split("}", 1)[1]
            # loop on element attributes also
            for an in el.attrib:
                if an.startswith("{"):
                    el.attrib[an.split("}", 1)[1]] = el.attrib.pop(an)
        # remove namespace declarations
        etree.cleanup_namespaces(self.root)

    def drop(self) -> None:
        """
        Drop matched nodes from the parent element.
        """
        try:
            parent = self.root.getparent()
        except AttributeError:
            # 'str' object has no attribute 'getparent'
            raise CannotRemoveElementWithoutRoot(
                "The node you're trying to drop has no root, "
                "are you trying to drop a pseudo-element? "
                "Try to use 'li' as a selector instead of 'li::text' or "
                "'//li' instead of '//li/text()', for example."
            )

        no_parent_message = (
            "The node you're trying to remove has no parent, "
            "are you trying to remove a root element?"
        )
        try:
            if self.type == "xml":
                if parent is None:
                    raise CannotDropElementWithoutParent(no_parent_message)
                # Unlike HtmlElement.drop_tree(), _Element.remove() also
                # removes the tail text, so we need to preserve it manually
                if self.root.tail:
                    previous = self.root.getprevious()
                    if previous is not None:
                        previous.tail = (previous.tail or "") + self.root.tail
                    else:
                        parent.text = (parent.text or "") + self.root.tail
                parent.remove(self.root)
            else:
                typing.cast("html.HtmlElement", self.root).drop_tree()
        except (AttributeError, AssertionError):
            # 'NoneType' object has no attribute 'drop_tree'
            raise CannotDropElementWithoutParent(no_parent_message)

    @property
    def attrib(self) -> dict[str, str]:
        """
        Return the attributes dictionary for underlying element.
        For JSON selectors, return an empty dict.
        """
        if self.type == "json":
            return {}

        return dict(self.root.attrib)

    def __bool__(self) -> bool:
        """
        Return ``True`` if there is any real content selected or ``False``
        otherwise.  In other words, the boolean value of a :class:`Selector` is
        given by the contents it selects.
        """
        return bool(self.get())

    __nonzero__ = __bool__

    def __str__(self) -> str:
        return str(self.get())

    def __repr__(self) -> str:
        data = repr(shorten(str(self.get()), width=40))
        return f"<{type(self).__name__} query={self._expr!r} data={data}>"
