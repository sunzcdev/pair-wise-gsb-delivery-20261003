from __future__ import annotations

import base64
import binascii
import json
from functools import lru_cache
from typing import TYPE_CHECKING, Any, Protocol, Self, cast

from cssselect import GenericTranslator as OriginalGenericTranslator
from cssselect import HTMLTranslator as OriginalHTMLTranslator
from cssselect.parser import (
    CombinedSelector,
    Element,
    Function,
    FunctionalPseudoElement,
    PseudoElement,
    Selector,
    Token,
    ascii_lower,
    parse,
    parse_series,
    tokenize,
)
from cssselect.xpath import ExpressionError, is_safe_name
from cssselect.xpath import XPathExpr as OriginalXPathExpr

if TYPE_CHECKING:
    from collections.abc import Iterable


#: Pseudo-classes that accept the ``An+B of S`` syntax of Selectors Level 4.
_NTH_OF_NAMES = frozenset({"nth-child", "nth-last-child"})

#: Pseudo-classes for which that syntax is unsupported and rejected.
_NTH_OF_REJECTED = frozenset({"nth-of-type", "nth-last-of-type"})

#: Name of the private pseudo-class that an ``An+B of S`` call is rewritten
#: to, so that the cssselect parser only ever sees a functional pseudo-class
#: with a single (base64-encoded) string argument.
_NTH_OF_PSEUDO = "parsel-nth-of"


class XPathExpr(OriginalXPathExpr):
    textnode: bool = False
    attribute: str | None = None

    @classmethod
    def from_xpath(
        cls,
        xpath: OriginalXPathExpr,
        textnode: bool = False,
        attribute: str | None = None,
    ) -> Self:
        x = cls(path=xpath.path, element=xpath.element, condition=xpath.condition)
        x.textnode = textnode
        x.attribute = attribute
        return x

    def __str__(self) -> str:
        path = super().__str__()
        if self.textnode:
            if path == "*":
                path = "text()"
            elif path.endswith("::*/*"):
                path = path[:-3] + "text()"
            else:
                path += "/text()"

        if self.attribute is not None:
            if path.endswith("::*/*"):
                path = path[:-2]
            if is_safe_name(self.attribute):
                path += f"/@{self.attribute}"
            else:
                literal = OriginalGenericTranslator.xpath_literal(self.attribute)
                path += f"/attribute::*[name() = {literal}]"

        return path

    def join(
        self: Self,
        combiner: str,
        other: OriginalXPathExpr,
        *args: Any,
        **kwargs: Any,
    ) -> Self:
        if not isinstance(other, XPathExpr):
            raise ValueError(  # noqa: TRY004
                f"Expressions of type {__name__}.XPathExpr can only join expressions"
                f" of the same type (or its descendants), got {type(other)}"
            )
        super().join(combiner, other, *args, **kwargs)
        self.textnode = other.textnode
        self.attribute = other.attribute
        return self


# e.g. cssselect.GenericTranslator, cssselect.HTMLTranslator
class TranslatorProtocol(Protocol):
    def xpath_element(self, selector: Element) -> OriginalXPathExpr: ...

    def css_to_xpath(self, css: str, prefix: str = ...) -> str: ...


#: Reverse axes used to test a selector against a context node.
_REVERSE_COMBINATOR = {
    " ": "ancestor::*",
    ">": "parent::*",
    "+": "preceding-sibling::*[1]",
    "~": "preceding-sibling::*",
}


def _split_nth_of(argument: str) -> tuple[str, str] | None:
    """Split the ``An+B of S`` argument of an ``:nth-*()`` pseudo-class.

    Return ``(series, selector_list)``, or ``None`` if there is no ``of``
    keyword. The split happens on the first top-level ``of`` identifier, so
    keywords, commas and parentheses inside strings (including attribute
    values) or nested functions are never mistaken for one.
    """
    depth = 0
    tokens = list(tokenize(argument))
    for index, token in enumerate(tokens):
        if token.is_delim("("):
            depth += 1
        elif token.is_delim(")"):
            depth -= 1
        elif (
            depth == 0
            and token.type == "IDENT"
            and ascii_lower(cast("str", token.value)) == "of"
        ):
            # Token positions are offsets into the original string, and the
            # next token starts right after this one (the EOF token starts at
            # the end of the argument). Using positions keeps escapes, whose
            # decoded value differs in length from their source, intact.
            return argument[: token.pos], argument[tokens[index + 1].pos :]
    return None


def _parse_nth_series(series: str) -> tuple[int, int]:
    # The An+B microsyntax is ASCII-case-insensitive. Older cssselect
    # versions do not lower-case it themselves, so do it here.
    tokens = [
        token
        for token in tokenize(ascii_lower(series))
        if token.type not in ("S", "EOF")
    ]
    try:
        return parse_series(tokens)
    except ValueError as exc:
        raise ExpressionError(f"Invalid series: {series!r}") from exc


def _nth_series_condition(count: str, a: int, b: int) -> str | None:
    """Return an XPath condition on ``count``, the number of matching
    siblings before (or after) the context node, for ``An+B``, or ``None``
    if the condition always holds.

    The position among the matching siblings is ``count + 1``, so the
    condition is ``count = a*n + b - 1`` for some integer ``n >= 0``. This
    mirrors cssselect's own :nth-child() arithmetic.
    """
    b_min_1 = b - 1
    if a == 1 and b_min_1 <= 0:
        return None
    if a < 0 and b_min_1 < 0:
        return "0"
    if a == 0:
        return f"{count} = {b_min_1}"
    expressions = []
    if a > 0:
        if b_min_1 > 0:
            expressions.append(f"{count} >= {b_min_1}")
    else:
        expressions.append(f"{count} <= {b_min_1}")
    if abs(a) != 1:
        b_neg = (-b_min_1) % abs(a)
        left = count if not b_neg else f"({count} +{b_neg})"
        expressions.append(f"{left} mod {a} = 0")
    template = "(%s)" if len(expressions) > 1 else "%s"
    return " and ".join(template % expression for expression in expressions)


def _has_scope(tree: Any) -> bool:
    """Return whether *tree* contains a ``:scope`` pseudo-class."""
    if getattr(tree, "ident", None) == "scope":
        return True
    for attribute in ("selector", "subselector"):
        child = getattr(tree, attribute, None)
        if child is not None and _has_scope(child):
            return True
    return False


def _validate_nth_of_selector_list(
    selector_list: Iterable[Selector], name: str
) -> None:
    """Reject selector lists that cannot be matched from a sibling node.

    This runs while translating, i.e. before the generated XPath is run, so
    unsupported combinations fail early and clearly instead of producing a
    query that silently selects the wrong nodes.
    """
    for selector in selector_list:
        if selector.pseudo_element is not None:
            raise ExpressionError(
                f"Pseudo-elements are not supported in the selector list of "
                f":{name}(An+B of S)"
            )
        if _has_scope(selector.parsed_tree):
            raise ExpressionError(
                f":scope is not supported in the selector list of :{name}(An+B of S)"
            )


def _encode_nth_of(name: str, series: str, selector_list: str) -> str:
    payload = json.dumps([name, series, selector_list])
    return base64.b64encode(payload.encode("utf-8")).decode("ascii")


def _decode_nth_of(value: str) -> tuple[str, str, str]:
    try:
        decoded = json.loads(base64.b64decode(value, validate=True))
    except (binascii.Error, ValueError, TypeError) as exc:
        raise ExpressionError(f"Invalid :{_NTH_OF_PSEUDO}() argument") from exc
    if (
        not isinstance(decoded, list)
        or len(decoded) != 3
        or not all(isinstance(item, str) for item in decoded)
    ):
        raise ExpressionError(f"Invalid :{_NTH_OF_PSEUDO}() argument")
    name, series, selector_list = decoded
    if name not in _NTH_OF_NAMES:
        raise ExpressionError(f"Invalid :{_NTH_OF_PSEUDO}() argument")
    return name, series, selector_list


def _rewrite_nth_of_argument(name: str, argument: str) -> str | None:
    """Rewrite one ``:nth-*(...)`` call, or return ``None`` to leave it as is.

    The selector list of ``of S`` is rewritten recursively (it may itself
    contain an ``of`` call) and the whole call is replaced by a private
    pseudo-class whose single string argument carries the pieces in a form
    the CSS tokenizer cannot mangle. Validation of the list happens when the
    private pseudo-class is translated, which is still before the generated
    XPath is run.
    """
    split = _split_nth_of(argument)
    if split is None:
        return None
    if name in _NTH_OF_REJECTED:
        raise ExpressionError(f":{name}(An+B of S) is not supported")
    series, selector_list = split
    if not selector_list.strip():
        raise ExpressionError(f"Expected a selector list after 'of' in :{name}()")
    _parse_nth_series(series)
    selector_list = _rewrite_nth_of(selector_list)
    encoded = _encode_nth_of(name, series.strip(), selector_list)
    return f":{_NTH_OF_PSEUDO}('{encoded}')"


def _nth_of_call_at(tokens: list[Token], index: int) -> bool:
    """Return whether tokens[index] starts a ``:nth-*(...)`` pseudo-class call."""
    token = tokens[index]
    if token.type != "IDENT":
        return False
    if ascii_lower(cast("str", token.value)) not in _NTH_OF_NAMES | _NTH_OF_REJECTED:
        return False
    if index < 1 or not tokens[index - 1].is_delim(":"):
        return False
    if index >= 2 and tokens[index - 2].is_delim(":"):
        # A "::" pseudo-element, not a pseudo-class.
        return False
    return index + 1 < len(tokens) and tokens[index + 1].is_delim("(")


def _rewrite_nth_of(css: str) -> str:
    """Rewrite ``:nth-child(An+B of S)`` and ``:nth-last-child(An+B of S)``
    into a private pseudo-class that cssselect can parse.

    The rest of the query is left untouched. Strings, escapes and comments
    are honored by working on the token stream, so nothing inside them (a
    comma in an attribute value, for example) is mistaken for the ``of``
    keyword or a selector-list separator.
    """
    tokens = list(tokenize(css))
    parts = []
    last = 0
    index = 0
    while index < len(tokens):
        if not _nth_of_call_at(tokens, index):
            index += 1
            continue
        depth = 1
        end = index + 2
        while end < len(tokens) and depth:
            if tokens[end].is_delim("("):
                depth += 1
            elif tokens[end].is_delim(")"):
                depth -= 1
            end += 1
        if depth:
            # Unbalanced parentheses: let the parser report the error.
            index += 1
            continue
        argument = css[tokens[index + 1].pos + 1 : tokens[end - 1].pos]
        rewritten = _rewrite_nth_of_argument(
            ascii_lower(cast("str", tokens[index].value)), argument
        )
        if rewritten is not None:
            parts.append(css[last : tokens[index - 1].pos])
            parts.append(rewritten)
            last = tokens[end - 1].pos + 1
            index = end
            continue
        index += 1
    parts.append(css[last:])
    return "".join(parts)


class TranslatorMixin:
    """This mixin adds support to CSS pseudo elements via dynamic dispatch.

    Currently supported pseudo-elements are ``::text`` and ``::attr(ATTR_NAME)``.
    """

    def __init__(self, *args: Any, **kwargs: Any):
        super().__init__(*args, **kwargs)
        self._build_cache()

    # The cache is per instance because translation depends on instance state,
    # and because a cache shared by all instances, keyed on the instance, keeps
    # every translator ever used alive.
    def _build_cache(self) -> None:
        self._cache = lru_cache(maxsize=256)(self._translate)

    def _translate(self, css: str, prefix: str) -> str:
        # https://github.com/python/mypy/issues/14757
        return super().css_to_xpath(  # type: ignore[misc,no-any-return]
            _rewrite_nth_of(css), prefix
        )

    def css_to_xpath(self, css: str, prefix: str = "descendant-or-self::") -> str:
        return self._cache(css, prefix)

    # The cache holds a reference to a bound method, which cannot be pickled.
    def __getstate__(self) -> dict[str, Any]:
        return {k: v for k, v in self.__dict__.items() if k != "_cache"}

    def __setstate__(self, state: dict[str, Any]) -> None:
        self.__dict__.update(state)
        self._build_cache()

    def xpath_element(self: TranslatorProtocol, selector: Element) -> XPathExpr:
        # https://github.com/python/mypy/issues/14757
        xpath = super().xpath_element(selector)  # type: ignore[safe-super]
        return XPathExpr.from_xpath(xpath)

    def xpath_pseudo_element(
        self, xpath: OriginalXPathExpr, pseudo_element: PseudoElement
    ) -> OriginalXPathExpr:
        """
        Dispatch method that transforms XPath to support pseudo-element
        """
        if isinstance(pseudo_element, FunctionalPseudoElement):
            method_name = f"xpath_{pseudo_element.name.replace('-', '_')}_functional_pseudo_element"
            method = getattr(self, method_name, None)
            if not method:
                raise ExpressionError(
                    f"The functional pseudo-element ::{pseudo_element.name}() is unknown"
                )
            xpath = method(xpath, pseudo_element)
        else:
            method_name = (
                f"xpath_{pseudo_element.replace('-', '_')}_simple_pseudo_element"
            )
            method = getattr(self, method_name, None)
            if not method:
                raise ExpressionError(
                    f"The pseudo-element ::{pseudo_element} is unknown"
                )
            xpath = method(xpath)
        return xpath

    def xpath_attr_functional_pseudo_element(
        self, xpath: OriginalXPathExpr, function: FunctionalPseudoElement
    ) -> XPathExpr:
        """Support selecting attribute values using ::attr() pseudo-element"""
        if function.argument_types() not in (["STRING"], ["IDENT"]):
            raise ExpressionError(
                f"Expected a single string or ident for ::attr(), got {function.arguments!r}"
            )
        return XPathExpr.from_xpath(xpath, attribute=function.arguments[0].value)

    def xpath_text_simple_pseudo_element(self, xpath: OriginalXPathExpr) -> XPathExpr:
        """Support selecting text nodes using ::text pseudo-element"""
        return XPathExpr.from_xpath(xpath, textnode=True)

    def xpath_parsel_nth_of_function(
        self, xpath: XPathExpr, function: Function
    ) -> XPathExpr:
        """Translate an ``An+B of S`` call rewritten to a private pseudo-class.

        Both the :nth-child() and :nth-last-child() variants end up here; the
        encoded payload records which one it was.
        """
        if function.argument_types() != ["STRING"]:
            raise ExpressionError(
                f"Expected a single string argument for :{_NTH_OF_PSEUDO}(), "
                f"got {function.arguments!r}"
            )
        name, series, selector_list = _decode_nth_of(
            cast("str", function.arguments[0].value)
        )
        a, b = _parse_nth_series(series)
        parsed = parse(selector_list)
        _validate_nth_of_selector_list(parsed, name)
        return self._xpath_nth_of(xpath, parsed, a, b, name=name)

    def _xpath_nth_of(
        self,
        xpath: XPathExpr,
        selector_list: Iterable[Selector],
        a: int,
        b: int,
        *,
        name: str,
    ) -> XPathExpr:
        """Limit *xpath* to the ``An+B``-th of its siblings matching the list.

        Only siblings matching the selector list are counted, a node matched
        by several selectors of the list counts once (the union is a single
        predicate), and every parent is counted separately, since the
        predicate only ever looks at the context node's own siblings.
        """
        match_condition = self._xpath_selector_list_condition(selector_list)
        last = name == "nth-last-child"
        axis = "following-sibling" if last else "preceding-sibling"
        if match_condition is None:
            # The list matches every element, so this is plain
            # :nth-child()/:nth-last-child(); only element siblings count.
            siblings = f"{axis}::*"
        else:
            siblings = f"{axis}::*[{match_condition}]"
        count_condition = _nth_series_condition(f"count({siblings})", a, b)
        conditions = []
        if match_condition is not None:
            # The element is indexed among the siblings matching S, so it has
            # to be one of them; otherwise the count alone would also match
            # non-matching elements sitting at a matching position.
            conditions.append(match_condition)
        if count_condition is not None:
            conditions.append(count_condition)
        if not conditions:
            return xpath
        return xpath.add_condition(" and ".join(f"({c})" for c in conditions))

    def _xpath_selector_list_condition(
        self, selector_list: Iterable[Selector]
    ) -> str | None:
        """Return a predicate that holds for the elements matching any of the
        selectors of *selector_list*, or ``None`` if that is every element.
        """
        conditions = []
        for selector in selector_list:
            condition = self._xpath_match_tree(selector.parsed_tree)
            if condition is None:
                return None
            conditions.append(condition)
        if len(conditions) == 1:
            return conditions[0]
        return " or ".join(f"({condition})" for condition in conditions)

    def _xpath_match_tree(self, tree: Any) -> str | None:
        """Return a predicate holding for the context element iff it matches
        *tree*, or ``None`` if *tree* matches every element.

        This matches a complex selector against the context node by walking
        it right to left over reverse axes, rather than building a location
        path, so the result can be embedded in a sibling predicate.
        """
        if isinstance(tree, CombinedSelector):
            axis = _REVERSE_COMBINATOR[tree.combinator]
            left = self._xpath_match_tree(tree.selector)
            if left is not None:
                axis = f"{axis}[{left}]"
            right = self._xpath_match_tree(tree.subselector)
            return axis if right is None else f"{right} and {axis}"
        sub_xpath = self.xpath(tree)  # type: ignore[attr-defined]
        if sub_xpath.path:
            raise ExpressionError(
                f"Unsupported selector in the 'of' selector list: {tree!r}"
            )
        sub_xpath.add_name_test()
        return sub_xpath.condition or None


class GenericTranslator(TranslatorMixin, OriginalGenericTranslator):
    pass


class HTMLTranslator(TranslatorMixin, OriginalHTMLTranslator):
    pass


_translator = HTMLTranslator()


def css2xpath(query: str) -> str:
    """Return translated XPath version of a given CSS query"""
    return _translator.css_to_xpath(query)
