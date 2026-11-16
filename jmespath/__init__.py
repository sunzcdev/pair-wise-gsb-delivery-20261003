from jmespath import parser
from jmespath import preparation
from jmespath.visitor import Options

__version__ = '1.1.0'


def compile(expression):
    return parser.Parser().parse(expression)


def prepare(expression, functions=None, options=None):
    """Statically check an expression against a function table.

    This is optional: ``compile`` never depends on a specific ``Functions``
    implementation.  ``prepare`` walks the parsed AST and raises
    ``UnknownFunctionError`` for functions that do not exist and
    ``ArityError``/``VariadictArityError`` for call sites whose argument
    count can never work.  It returns a
    ``jmespath.preparation.PreparedExpression`` describing the functions the
    expression needs and the argument types that can only be confirmed at
    run time.

    ``expression`` may be a JMESPath string or a result from ``compile``.
    """
    return preparation.prepare(
        expression, functions=functions, options=options)


def search(expression, data, options=None):
    return parser.Parser().parse(expression).search(data, options=options)
