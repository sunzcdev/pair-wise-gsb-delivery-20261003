"""XPath and JMESPath selectors based on the lxml and jmespath Python
packages."""

from __future__ import annotations

import codecs
import json
import re
import typing
import warnings
from dataclasses import dataclass
from enum import StrEnum
from functools import lru_cache
from io import BytesIO
from typing import (
    TYPE_CHECKING,
    Any,
    Literal,
    Protocol,
    Self,
    SupportsIndex,
    TypeAlias,
    TypedDict,
    TypeVar,
)

import jmespath
from cssselect.parser import SelectorError as CSSSelectorError
from lxml import etree, html

from .csstranslator import GenericTranslator, HTMLTranslator
from .utils import extract_regex, flatten, iflatten, shorten

if TYPE_CHECKING:
    from collections.abc import Callable, Mapping, Sequence
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
    """

    __slots__ = [
        "__weakref__",
        "_expr",
        "_huge_tree",
        "_text",
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
    ) -> None:
        self.root: Any
        if type not in ("html", "json", "text", "xml", None):
            raise ValueError(f"Invalid type: {type}")

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

    def __getstate__(self) -> Any:
        raise TypeError("can't pickle Selector objects")

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
            if isinstance(x, str):
                return self.__class__(text=x, _expr=query, type="text")
            return self.__class__(root=x, _expr=query)

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
    ) -> SelectorList[Self]:
        if self.type not in ("html", "xml", "text"):
            raise ValueError(f"Cannot use xpath on a Selector of type {self.type!r}")
        if self.type in ("html", "xml"):
            root = self.root
            if not hasattr(root, "xpath"):
                if isinstance(root, str) and query.strip() == ".":
                    return typing.cast(
                        "SelectorList[Self]",
                        self.selectorlist_cls(
                            [
                                self.__class__(
                                    root=self.root,
                                    _expr=query,
                                    namespaces=self.namespaces,
                                    type=self.type,
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
            xpathev = _get_xpath_evaluator(query, nsp, self._lxml_smart_strings)
            result = xpathev(root, **kwargs)
        except etree.XPathError as exc:
            raise ValueError(f"XPath error: {exc} in {query}")

        if not isinstance(result, list):
            result = [result]

        result = [
            self.__class__(
                root=x,
                _expr=query,
                namespaces=self.namespaces,
                type=_xml_or_html(self.type),
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
        return self.xpath(self._css2xpath(query))

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


# ---------------------------------------------------------------------------
# Candidate rules
# ---------------------------------------------------------------------------


class RuleError(Exception):
    """Base class for the errors reported by the candidate-rule helpers."""


class RuleConfigurationError(RuleError):
    """A rule, or a call of :func:`select_first`, is configured incorrectly."""


class RuleSyntaxError(RuleConfigurationError):
    """The XPath or CSS expression of a rule is invalid.

    Rule expressions are configuration, so an invalid expression is reported
    as a rule error instead of being treated as a candidate without matches.
    """


class RuleStatus(StrEnum):
    """The outcome of evaluating an ordered sequence of rules.

    The string values are stable and suitable for logs and metrics.
    """

    MATCHED = "matched"
    NO_MATCH = "no_match"
    ERROR = "error"


class IssueCategory(StrEnum):
    """The kind of problem that a rule result represents.

    * :attr:`TEMPLATE_MISMATCH` means that a record node did not contain the
      nodes that a candidate expected, which usually points to a page
      template change.
    * :attr:`INVALID_CONTENT` means that a candidate matched nodes, but their
      content was not acceptable, for example because a value was empty or
      because the rule validator rejected it.
    * :attr:`RULE_ERROR` means that the rule itself is broken, for example by
      an invalid expression or by a validator that raised.

    The string values are stable and suitable for logs and metrics.
    """

    TEMPLATE_MISMATCH = "template_mismatch"
    INVALID_CONTENT = "invalid_content"
    RULE_ERROR = "rule_error"


class RejectionReason(StrEnum):
    """Why a candidate rule was rejected.

    The string values are stable and suitable for logs and metrics.
    """

    NO_MATCH = "no_match"
    TOO_MANY_MATCHES = "too_many_matches"
    EMPTY_VALUE = "empty_value"
    VALIDATOR_REJECTED = "validator_rejected"
    VALIDATOR_ERROR = "validator_error"


_REJECTION_CATEGORIES: dict[RejectionReason, IssueCategory] = {
    RejectionReason.NO_MATCH: IssueCategory.TEMPLATE_MISMATCH,
    RejectionReason.TOO_MANY_MATCHES: IssueCategory.TEMPLATE_MISMATCH,
    RejectionReason.EMPTY_VALUE: IssueCategory.INVALID_CONTENT,
    RejectionReason.VALIDATOR_REJECTED: IssueCategory.INVALID_CONTENT,
    RejectionReason.VALIDATOR_ERROR: IssueCategory.RULE_ERROR,
}


class ValidatorErrorPolicy(StrEnum):
    """What :func:`select_first` does when a rule validator raises.

    * :attr:`STOP` stops evaluating the remaining candidates and returns a
      result whose status is :attr:`RuleStatus.ERROR`, because an unexpected
      exception usually means that the validator is broken.
    * :attr:`FAIL_CANDIDATE` records the exception as a rejection of that
      candidate and continues with the next candidate.
    """

    STOP = "stop"
    FAIL_CANDIDATE = "fail_candidate"


class _SupportsSelection(Protocol):
    """The part of the :class:`Selector` interface that rules rely on."""

    def xpath(
        self,
        query: str,
        namespaces: Mapping[str, str] | None = None,
        **kwargs: Any,
    ) -> SelectorList[Selector]: ...

    def css(self, query: str) -> SelectorList[Selector]: ...


def _check_node(node: object) -> None:
    """Make sure *node* is a single record node, not a list of nodes."""
    if isinstance(node, (list, tuple, SelectorList)):
        msg = (
            f"select_first() expects a single record node, got"
            f" {type(node).__name__}; iterate over the records and call"
            f" select_first() for each one"
        )
        raise RuleConfigurationError(msg)


def _check_count(name: str, value: object) -> int:
    """Validate a match-count bound and return it as an ``int``."""
    if isinstance(value, bool) or not isinstance(value, int):
        msg = f"{name} must be an integer, got {value!r}"
        raise RuleConfigurationError(msg)
    if value < 0:
        msg = f"{name} must be equal to or greater than 0, got {value}"
        raise RuleConfigurationError(msg)
    return value


@dataclass(frozen=True)
class Rule:
    """A candidate rule for extracting the values of a field.

    ``query`` is an XPath or CSS expression evaluated on the record node.
    XPath is used by default; pass ``method="css"`` to use CSS instead.

    ``min_count`` and ``max_count`` bound the number of nodes that ``query``
    must match for the rule to accept the result. A rule matches exactly one
    node by default; set ``max_count=None`` to accept any number of nodes, or
    ``min_count=0`` to accept a missing value.

    ``allow_empty`` decides whether empty strings among the extracted values
    are acceptable. It is a decision of each rule, so no blanket truthiness
    test is applied: only the exact empty string counts as empty, whereas
    values such as ``"0"`` or ``" "`` are content. When ``allow_empty`` is
    ``False`` (the default), a candidate whose values include an empty string
    is rejected with :attr:`RejectionReason.EMPTY_VALUE`.

    ``validator`` is an optional callable that receives the extracted values
    as a tuple of strings. It accepts the candidate by returning ``True``.
    Any other return value rejects the candidate, and a string return value is
    recorded as the rejection detail.

    ``name`` optionally identifies the rule in reports; ``method:query`` is
    used otherwise.
    """

    query: str
    method: str = "xpath"
    min_count: int = 1
    max_count: int | None = 1
    allow_empty: bool = False
    validator: Callable[[tuple[str, ...]], object] | None = None
    name: str | None = None

    def __post_init__(self) -> None:
        if self.method not in ("xpath", "css"):
            msg = f"Invalid method: {self.method!r}; expected 'xpath' or 'css'"
            raise RuleConfigurationError(msg)
        if not isinstance(self.query, str):
            msg = f"query must be a string, got {type(self.query).__name__}"
            raise RuleConfigurationError(msg)
        if not self.query.strip():
            msg = "query must not be empty"
            raise RuleConfigurationError(msg)
        _check_count("min_count", self.min_count)
        if self.max_count is not None:
            _check_count("max_count", self.max_count)
            if self.max_count < self.min_count:
                msg = (
                    f"max_count ({self.max_count}) must be equal to or greater"
                    f" than min_count ({self.min_count})"
                )
                raise RuleConfigurationError(msg)
        if not isinstance(self.allow_empty, bool):
            msg = f"allow_empty must be a bool, got {self.allow_empty!r}"
            raise RuleConfigurationError(msg)
        if self.validator is not None and not callable(self.validator):
            msg = f"validator must be callable, got {self.validator!r}"
            raise RuleConfigurationError(msg)

    def label(self) -> str:
        """Return a readable identifier of this rule for reports."""
        if self.name is not None:
            return self.name
        return f"{self.method}:{self.query}"

    def expected_count(self) -> str:
        """Describe the number of matches that this rule accepts."""
        if self.max_count is None:
            return f"at least {self.min_count}"
        if self.min_count == self.max_count:
            return f"exactly {self.min_count}"
        return f"between {self.min_count} and {self.max_count}"

    def matches_count(self, count: int) -> bool:
        """Return whether *count* is within the accepted match range."""
        if count < self.min_count:
            return False
        return self.max_count is None or count <= self.max_count

    def validate(self) -> None:
        """Check the syntax of the expression without evaluating it.

        Scraping code can call this to reject a broken ruleset before it
        starts scanning records. Invalid expressions raise
        :class:`RuleSyntaxError`.
        """
        try:
            if self.method == "xpath":
                etree.XPath(self.query)
            else:
                # The HTML translator accepts every CSS expression that the
                # XML translator does, plus the HTML-only pseudo-classes.
                _ctgroup["html"]["_csstranslator"].css_to_xpath(self.query)
        except (etree.XPathError, CSSSelectorError) as exc:
            msg = (
                f"Invalid {self.method} expression {self.query!r} in rule"
                f" {self.label()!r}: {exc}"
            )
            raise RuleSyntaxError(msg) from exc


@dataclass(frozen=True)
class Rejection:
    """A candidate rule that was rejected, with the reason why.

    ``index`` is the position of the rule in the sequence given to
    :func:`select_first`, ``count`` is the number of nodes that its expression
    matched, and ``detail`` is a human-readable explanation.
    """

    index: int
    rule: Rule
    reason: RejectionReason
    count: int = 0
    detail: str = ""

    @property
    def category(self) -> IssueCategory:
        """The kind of problem that this rejection represents."""
        return _REJECTION_CATEGORIES[self.reason]

    def describe(self) -> str:
        """Return a one-line description of this rejection."""
        text = (
            f"#{self.index} [{self.category.value}] {self.rule.label()}: "
            f"{self.reason.value}"
        )
        if self.detail:
            text += f" ({self.detail})"
        return text


@dataclass(frozen=True)
class RuleResult:
    """The outcome of :func:`select_first`.

    ``status`` tells whether a rule matched, no rule matched, or a rule itself
    was broken. ``rules`` holds the configured candidates in order.

    On a match, ``index``, :attr:`rule` and ``values`` identify the winning
    candidate and hold its extracted strings. ``rejections`` records every
    candidate that was tried and rejected beforehand, in configuration order,
    whether or not a later candidate matched. On an error, ``index`` and
    :attr:`rule` identify the broken candidate, and ``error`` holds the
    :class:`RuleError` that stopped the evaluation.
    """

    status: RuleStatus
    rules: tuple[Rule, ...]
    index: int | None = None
    values: tuple[str, ...] = ()
    rejections: tuple[Rejection, ...] = ()
    error: RuleError | None = None

    @property
    def matched(self) -> bool:
        """Whether a rule matched."""
        return self.status is RuleStatus.MATCHED

    @property
    def rule(self) -> Rule | None:
        """The rule that matched or reported an error, if any."""
        if self.index is None:
            return None
        return self.rules[self.index]

    @property
    def value(self) -> str | None:
        """The first extracted value, or ``None`` if there is none."""
        return self.values[0] if self.values else None

    @property
    def category(self) -> IssueCategory | None:
        """The kind of problem that the result represents, if any.

        A matching result has no problem. An error result is always a
        :attr:`IssueCategory.RULE_ERROR`. A result without a match reports the
        most severe problem among its rejections: a rule error first, then
        invalid content, and a template mismatch otherwise.
        """
        if self.status is RuleStatus.MATCHED:
            return None
        if self.status is RuleStatus.ERROR:
            return IssueCategory.RULE_ERROR
        if self.rule_errors:
            return IssueCategory.RULE_ERROR
        if self.invalid_content:
            return IssueCategory.INVALID_CONTENT
        return IssueCategory.TEMPLATE_MISMATCH

    def rejections_with_category(
        self, category: IssueCategory
    ) -> tuple[Rejection, ...]:
        """Return the rejections that represent *category*."""
        return tuple(r for r in self.rejections if r.category is category)

    @property
    def template_mismatches(self) -> tuple[Rejection, ...]:
        """Rejections caused by nodes that the record did not contain."""
        return self.rejections_with_category(IssueCategory.TEMPLATE_MISMATCH)

    @property
    def invalid_content(self) -> tuple[Rejection, ...]:
        """Rejections caused by matched nodes with unacceptable content."""
        return self.rejections_with_category(IssueCategory.INVALID_CONTENT)

    @property
    def rule_errors(self) -> tuple[Rejection, ...]:
        """Rejections caused by a broken rule, such as a raising validator."""
        return self.rejections_with_category(IssueCategory.RULE_ERROR)

    def explain(self) -> str:
        """Return a multi-line report of the outcome.

        Rejected candidates are prefixed with their :class:`IssueCategory`,
        so that a template mismatch, invalid content and a rule error can be
        told apart in logs.
        """
        rule = self.rule
        if self.status is RuleStatus.MATCHED:
            label = rule.label() if rule is not None else "?"
            headline = (
                f"Matched rule #{self.index} ({label})"
                f" with {len(self.values)} value(s)."
            )
            lines = [headline]
        elif self.status is RuleStatus.ERROR:
            where = (
                f"rule #{self.index} ({rule.label()})" if rule is not None else "a rule"
            )
            headline = (
                f"[{IssueCategory.RULE_ERROR.value}] Rule error in {where}: "
                f"{self.error}"
            )
            lines = [headline]
        else:
            category = self.category or IssueCategory.TEMPLATE_MISMATCH
            lines = [f"[{category.value}] No rule matched."]
        if self.rejections:
            lines.append("Rejected candidates:")
            lines.extend(f"  {rejection.describe()}" for rejection in self.rejections)
        return "\n".join(lines)


def _is_nested(node: _SupportsSelection) -> bool:
    root = getattr(node, "root", None)
    return isinstance(root, etree._Element) and root.getparent() is not None


def _run_query(node: _SupportsSelection, rule: Rule) -> SelectorList[Selector]:
    """Evaluate the expression of *rule* on *node*.

    A rule expression that the node cannot evaluate is a configuration
    problem, so the corresponding exception becomes a :class:`RuleSyntaxError`
    instead of being treated as a candidate without matches. Other exceptions,
    which signal a bug elsewhere, are not swallowed.
    """
    method = getattr(node, rule.method, None)
    if not callable(method):
        msg = (
            f"The record node has no {rule.method}() method, so rule"
            f" {rule.label()!r} cannot be evaluated"
        )
        raise RuleConfigurationError(msg)
    node_type = getattr(node, "type", None)
    if node_type not in (None, "html", "xml", "text"):
        # Selector.xpath()/css() cannot run on, say, a JSON selector, and its
        # ValueError would otherwise be reported as an expression syntax
        # error even though the expression may be perfectly valid.
        msg = (
            f"The record node has type {node_type!r}, so {rule.method} rules"
            f" such as {rule.label()!r} cannot be evaluated on it"
        )
        raise RuleConfigurationError(msg)
    if rule.method == "xpath" and rule.query[:1] == "/" and _is_nested(node):
        # A syntax error takes precedence over the scoping problem, so a typo
        # is never described as an absolute-path mistake.
        rule.validate()
        msg = (
            f"Rule {rule.label()!r} uses the document-scoped expression"
            f" {rule.query!r} on a nested record node; use a relative"
            f" expression such as './/...' instead"
        )
        raise RuleConfigurationError(msg)
    try:
        selected = method(rule.query)
    except (etree.XPathError, ValueError, CSSSelectorError) as exc:
        msg = (
            f"Invalid {rule.method} expression {rule.query!r} in rule"
            f" {rule.label()!r}: {exc}"
        )
        raise RuleSyntaxError(msg) from exc
    return typing.cast("SelectorList[Selector]", selected)


def _validator_error(rule: Rule, exc: Exception) -> RuleError:
    return RuleError(
        f"Validator of rule {rule.label()!r} raised {type(exc).__name__}: {exc}"
    )


def _check_rules(rules: object) -> tuple[Rule, ...]:
    """Validate the *rules* argument of :func:`select_first`."""
    if isinstance(rules, Rule):
        msg = "rules must be a sequence of Rule objects, not a single Rule"
        raise RuleConfigurationError(msg)
    if isinstance(rules, (str, bytes)) or not hasattr(rules, "__iter__"):
        msg = f"rules must be a sequence of Rule objects, got {rules!r}"
        raise RuleConfigurationError(msg)
    configured = tuple(typing.cast("Sequence[object]", rules))
    if not configured:
        msg = "at least one rule is required"
        raise RuleConfigurationError(msg)
    for rule in configured:
        if not isinstance(rule, Rule):
            msg = f"Expected a Rule, got {type(rule).__name__}"
            raise RuleConfigurationError(msg)
    return typing.cast("tuple[Rule, ...]", configured)


def _apply_validator(
    rule: Rule,
    values: tuple[str, ...],
    index: int,
    count: int,
    policy: ValidatorErrorPolicy,
) -> Rejection | None:
    """Run the validator of *rule*.

    Returns ``None`` when the candidate is accepted, a rejection when it is
    rejected, and raises :class:`RuleError` when the policy is
    :attr:`ValidatorErrorPolicy.STOP` and the validator raises.
    """
    if rule.validator is None:
        return None
    try:
        verdict = rule.validator(values)
    except Exception as exc:  # pylint: disable=broad-exception-caught
        # A validator is caller code, so any exception it raises must follow
        # the validator error policy instead of escaping this helper.
        if policy is ValidatorErrorPolicy.STOP:
            raise _validator_error(rule, exc) from exc
        return Rejection(
            index=index,
            rule=rule,
            reason=RejectionReason.VALIDATOR_ERROR,
            count=count,
            detail=f"{type(exc).__name__}: {exc}",
        )
    if verdict is True:
        return None
    return Rejection(
        index=index,
        rule=rule,
        reason=RejectionReason.VALIDATOR_REJECTED,
        count=count,
        detail=verdict if isinstance(verdict, str) else "",
    )


def _evaluate(
    node: _SupportsSelection,
    rule: Rule,
    index: int,
    policy: ValidatorErrorPolicy,
) -> tuple[tuple[str, ...], Rejection | None]:
    """Evaluate one candidate on *node*.

    Returns the extracted values and ``None`` when the candidate wins, or an
    empty tuple and the rejection that explains why it did not. A rule
    problem raises :class:`RuleError`.
    """
    selected = _run_query(node, rule)
    count = len(selected)
    if not rule.matches_count(count):
        reason = (
            RejectionReason.NO_MATCH
            if count < rule.min_count
            else RejectionReason.TOO_MANY_MATCHES
        )
        return (), Rejection(
            index=index,
            rule=rule,
            reason=reason,
            count=count,
            detail=f"expected {rule.expected_count()} match(es), got {count}",
        )

    values = tuple(selected.getall())
    empty = sum(value == "" for value in values)
    if empty and not rule.allow_empty:
        return (), Rejection(
            index=index,
            rule=rule,
            reason=RejectionReason.EMPTY_VALUE,
            count=count,
            detail=f"{empty} of {count} value(s) are empty",
        )

    rejection = _apply_validator(rule, values, index, count, policy)
    if rejection is not None:
        return (), rejection
    return values, None


def select_first(
    node: _SupportsSelection,
    rules: Sequence[Rule],
    *,
    on_validator_error: ValidatorErrorPolicy | str = ValidatorErrorPolicy.STOP,
) -> RuleResult:
    """Try *rules* in order on the single record node *node*.

    Each candidate is evaluated with the ``xpath()`` or ``css()`` method of
    *node*, in the order in which the rules are given. The first candidate
    wins whose expression matches a number of nodes within the ``min_count``
    and ``max_count`` range of its rule, whose extracted values are acceptable
    to ``allow_empty``, and whose optional ``validator`` returns ``True``.
    Its values are returned, and the candidates tried before it are reported
    in ``rejections`` together with the reason why each one was rejected.

    Empty values are not checked with a truthiness test: only the exact empty
    string counts as empty, and whether it is acceptable is decided by the
    ``allow_empty`` flag of each rule.

    Invalid XPath or CSS syntax is a configuration problem, not a missing
    match: the evaluation stops and the result status is
    :attr:`RuleStatus.ERROR`, with a :class:`RuleSyntaxError` in ``error``.

    ``on_validator_error`` decides what happens when a validator raises.
    :attr:`ValidatorErrorPolicy.STOP` (the default) stops with a
    :class:`RuleError`; :attr:`ValidatorErrorPolicy.FAIL_CANDIDATE` records
    the exception as a rejection and tries the next candidate.

    The function only reads *node*: it does not modify the DOM, it does not
    access the network, and it does not depend on where the node comes from.

    >>> from parsel import Rule, select_first
    >>> record = Selector(text="<div><h2>Red shoes</h2></div>").css("div")[0]
    >>> rules = [
    ...     Rule("h1::text", method="css", name="old"),
    ...     Rule("h2::text", method="css", name="new"),
    ... ]
    >>> result = select_first(record, rules)
    >>> result.status.value
    'matched'
    >>> result.values
    ('Red shoes',)
    >>> result.rejections[0].reason.value
    'no_match'
    >>> result.rejections[0].category.value
    'template_mismatch'
    """
    configured = _check_rules(rules)
    _check_node(node)
    try:
        policy = ValidatorErrorPolicy(on_validator_error)
    except ValueError:
        msg = (
            f"Invalid on_validator_error: {on_validator_error!r}; expected"
            f" 'stop' or 'fail_candidate'"
        )
        raise RuleConfigurationError(msg) from None

    rejections: list[Rejection] = []
    for index, rule in enumerate(configured):
        try:
            values, rejection = _evaluate(node, rule, index, policy)
        except RuleError as exc:
            return RuleResult(
                status=RuleStatus.ERROR,
                rules=configured,
                index=index,
                rejections=tuple(rejections),
                error=exc,
            )
        if rejection is not None:
            rejections.append(rejection)
            continue
        return RuleResult(
            status=RuleStatus.MATCHED,
            rules=configured,
            index=index,
            values=values,
            rejections=tuple(rejections),
        )

    return RuleResult(
        status=RuleStatus.NO_MATCH,
        rules=configured,
        rejections=tuple(rejections),
    )
