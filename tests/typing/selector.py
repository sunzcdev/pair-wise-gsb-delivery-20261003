# Basic usage of the Selector, strongly typed to test the typing of parsel's API.
from __future__ import annotations

import re

from parsel import Selector
from parsel.selector import (
    IssueCategory,
    RejectionReason,
    Rule,
    RuleResult,
    RuleStatus,
    ValidatorErrorPolicy,
    select_first,
)


def correct() -> None:
    selector = Selector(
        text="<html><body><ul><li>1</li><li>2</li><li>3</li></ul></body></html>"
    )

    li_values: list[str] = selector.css("li").getall()
    selector.re_first(re.compile(r"[32]"), "").strip()
    xpath_values: list[str] = selector.xpath(
        "//somens:a/text()", namespaces={"somens": "http://scrapy.org"}
    ).extract()

    class MySelector(Selector):
        def my_own_func(self) -> int:
            return 3

    my_selector = MySelector()
    res: int = my_selector.my_own_func()
    sub_res: int = my_selector.xpath("//somens:a/text()")[0].my_own_func()
    jsonpath_res: int = my_selector.jsonpath("$.a")[0].my_own_func()


# Negative checks: all the code lines below have typing errors.
# the "# type: ignore" comment makes sure that mypy identifies them as errors.


def incorrect() -> None:
    selector = Selector(
        text="<html><body><ul><li>1</li><li>2</li><li>3</li></ul></body></html>"
    )

    # Wrong query type in css.
    selector.css(5).getall()  # type: ignore[arg-type]

    # Cannot assign a list of str to an int.
    li_values: int = selector.css("li").getall()  # type: ignore[assignment]

    # Cannot use a string to define namespaces in xpath.
    selector.xpath(
        "//somens:a/text()",
        namespaces='{"somens": "http://scrapy.org"}',  # type: ignore[arg-type]
    ).extract()

    # Typo in the extract method name.
    selector.css("li").extact()  # type: ignore[attr-defined]

    class MySelector(Selector):
        def my_own_func(self) -> int:
            return 3

    my_selector = MySelector()
    res: str = my_selector.my_own_func()  # type: ignore[assignment]
    sub_res: str = my_selector.xpath("//somens:a/text()")[0].my_own_func()  # type: ignore[assignment]


def rules_correct() -> None:
    record = Selector(text="<div><span>x</span></div>").css("div")[0]
    rules = [
        Rule("h1::text", method="css", name="primary"),
        Rule(
            ".//span/text()",
            min_count=1,
            max_count=3,
            allow_empty=True,
            validator=lambda values: True,
        ),
    ]
    result: RuleResult = select_first(record, rules)
    values: tuple[str, ...] = result.values
    status: RuleStatus = result.status
    single: str | None = result.value
    _ = (values, status, single)
    if result.error is not None:
        _ = str(result.error)
    for rejection in result.rejections:
        reason: RejectionReason = rejection.reason
        category: IssueCategory = rejection.category
        _ = (reason.value, category.value)
    select_first(record, rules, on_validator_error=ValidatorErrorPolicy.STOP)
    select_first(record, rules, on_validator_error="fail_candidate")


def rules_incorrect() -> None:
    record = Selector(text="<div/>")

    # The query must be a string.
    Rule(5)  # type: ignore[arg-type]

    # A validator receives the tuple of values, not a single string.
    def one_value_validator(value: str) -> bool:
        return value.startswith("$")

    Rule("h1", validator=one_value_validator)  # type: ignore[arg-type]

    # select_first needs a sequence of rules, not a single rule.
    select_first(record, Rule("h1"))  # type: ignore[arg-type]

    # values is a tuple of strings, not a list.
    result = select_first(record, [Rule("h1")])
    values: list[str] = result.values  # type: ignore[assignment]
    _ = values
