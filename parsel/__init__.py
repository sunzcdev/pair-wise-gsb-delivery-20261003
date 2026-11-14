"""
Parsel lets you extract text from XML/HTML documents using XPath
or CSS selectors
"""

__author__ = "Scrapy project"
__email__ = "info@scrapy.org"
__version__ = "1.12.1"
__all__ = [
    "AbsoluteXPathWarning",
    "Selector",
    "SelectorList",
    "TableCell",
    "TableDiagnostic",
    "TableGrid",
    "TableRowGroup",
    "css2xpath",
    "parse_table",
    "xpathfuncs",
]

from parsel import xpathfuncs
from parsel.csstranslator import css2xpath
from parsel.selector import AbsoluteXPathWarning, Selector, SelectorList
from parsel.table import (
    TableCell,
    TableDiagnostic,
    TableGrid,
    TableRowGroup,
    parse_table,
)

xpathfuncs.setup()
