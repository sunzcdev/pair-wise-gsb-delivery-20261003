"""
Parsel lets you extract text from XML/HTML documents using XPath
or CSS selectors
"""

__author__ = "Scrapy project"
__email__ = "info@scrapy.org"
__version__ = "1.12.1"
__all__ = [
    "AbsoluteXPathWarning",
    "IssueCategory",
    "Rejection",
    "RejectionReason",
    "Rule",
    "RuleConfigurationError",
    "RuleError",
    "RuleResult",
    "RuleStatus",
    "RuleSyntaxError",
    "Selector",
    "SelectorList",
    "ValidatorErrorPolicy",
    "css2xpath",
    "select_first",
    "xpathfuncs",
]

from parsel import xpathfuncs
from parsel.csstranslator import css2xpath
from parsel.selector import (
    AbsoluteXPathWarning,
    IssueCategory,
    Rejection,
    RejectionReason,
    Rule,
    RuleConfigurationError,
    RuleError,
    RuleResult,
    RuleStatus,
    RuleSyntaxError,
    Selector,
    SelectorList,
    ValidatorErrorPolicy,
    select_first,
)

xpathfuncs.setup()
