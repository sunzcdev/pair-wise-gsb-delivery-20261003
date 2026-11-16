# Tests splitting SQL streams fed in chunks.

import pytest

import sqlparse
from sqlparse.engine import StreamStatementSplitter


def chunked(text, size):
    return [text[i:i + size] for i in range(0, len(text), size)]


SAMPLES = [
    'select * from foo;',
    "select * from foo where bar = 'foo;bar';",
    "select '\'; select '\'';",
    "select 'it''s a test'; select 2;",
    'select $$dollar;quoted$$; select 2;',
    'select $tag$a;b$tag$; select 2;',
    'select 1; /* comment ; */ select 2;',
    'select 1; /*+ hint ; */ select 2;',
    'select 1; -- line comment\nselect 2;',
    'select 1; -- line comment\r\nselect 2;',
    'CREATE OR REPLACE FUNCTION foo() RETURNS void AS $$\n'
    'BEGIN\n  select 1;\nEND;\n$$;',
    'BEGIN TRANSACTION; select 1;',
    'select `back``tick`, [bracket name];',
    'select %(name)s, %s, ?1, :named, $1;',
    'select 1.5, .5, 1., 0x1F, -3, 1E5, 1.5E-2, -0x1F;',
    'select @var, #temp, ##global, @>x, #>y;',
    'select a . b, t . x from t;',
    'select count(*) from t;',
    'select 1 GO 2\nselect 2 GO',
    'select "sym""bol";',
    'IF NOT EXISTS (select 1) select 2;',
    'select 1;; select 2;',
    'select 1; select 2',
    'select 1',
    '',
    '   ',
    '-- only a comment',
    '/* only a comment */',
]


@pytest.mark.parametrize('sql', SAMPLES)
@pytest.mark.parametrize('size', [1, 2, 3, 5, 7])
def test_splitstream_matches_split(sql, size):
    expected = sqlparse.split(sql)
    assert list(sqlparse.splitstream(chunked(sql, size))) == expected


@pytest.mark.parametrize('sql', SAMPLES)
def test_splitstream_all_split_points(sql):
    expected = sqlparse.split(sql)
    for i in range(len(sql) + 1):
        chunks = [sql[:i], sql[i:]]
        assert list(sqlparse.splitstream(chunks)) == expected


def test_splitstream_files(load_file):
    for fn in ['begintag.sql', 'dashcomment.sql', 'function_psql.sql',
               'function_psql2.sql', 'mysql_handler.sql',
               'multiple_case_in_begin.sql']:
        sql = load_file(fn)
        expected = sqlparse.split(sql)
        for size in [1, 3, 17]:
            assert list(sqlparse.splitstream(chunked(sql, size))) == expected


def test_splitstream_cut_at_escape():
    sql = "select '\'; select '\'';"
    expected = sqlparse.split(sql)
    for i in range(len(sql) + 1):
        assert list(sqlparse.splitstream([sql[:i], sql[i:]])) == expected


def test_splitstream_cut_at_semicolon():
    sql = 'select 1;select 2;'
    expected = sqlparse.split(sql)
    for i in range(len(sql) + 1):
        assert list(sqlparse.splitstream([sql[:i], sql[i:]])) == expected


def test_splitstream_produces_complete_statements():
    splitter = StreamStatementSplitter()
    # "sel" may still grow into a larger token, so the first statement
    # is delivered once the next statement's first token is decidable.
    assert splitter.feed('select 1; sel') == []
    assert splitter.feed('ect 2;') == ['select 1;']
    assert splitter.feed(' select 3') == ['select 2;']
    result = splitter.close()
    assert result.statements == []
    assert result.statement == 'select 3'
    assert result.complete


def test_splitstream_trailing_comment_stays_with_statement():
    splitter = StreamStatementSplitter()
    assert splitter.feed('select 1; -- done\n') == []
    assert splitter.feed('select 2;') == ['select 1; -- done']
    result = splitter.close()
    assert result.statement == 'select 2;'


@pytest.mark.parametrize('sql,reason', [
    ("select 'unclosed", 'unclosed string literal'),
    ('select "unclosed', 'unclosed string literal'),
    ('select `unclosed', 'unclosed quoted name'),
    ('select /* unclosed', 'unclosed multiline comment'),
    ('select $$unclosed', 'unclosed dollar-quoted literal'),
    ('select $tag$unclosed', 'unclosed dollar-quoted literal'),
    ('select (1', 'unbalanced parenthesis'),
    ('CREATE FUNCTION f() AS $$ BEGIN select 1;', "unclosed block 'BEGIN'"),
])
def test_splitstream_incomplete_status(sql, reason):
    splitter = StreamStatementSplitter()
    splitter.feed(sql)
    result = splitter.close()
    assert not result.complete
    assert reason in result.reason
    # the trailing fragment is still delivered, like split() does
    assert result.statement == sqlparse.split(sql)[0]


@pytest.mark.parametrize('sql', [
    'select 1;',
    'select 1',
    'select 1; -- trailing comment',
    'select (1);',
    'select $$closed$$;',
])
def test_splitstream_complete_status(sql):
    splitter = StreamStatementSplitter()
    splitter.feed(sql)
    result = splitter.close()
    assert result.complete
    assert result.reason is None


def test_splitstream_keeps_only_current_statement():
    splitter = StreamStatementSplitter()
    for i in range(1000):
        splitter.feed('select %d; ' % i)
    # delivered statements are not accumulated
    assert len(splitter._splitter.tokens) <= 4
    result = splitter.close()
    assert result.complete


def test_splitstream_feed_after_close():
    splitter = StreamStatementSplitter()
    splitter.close()
    with pytest.raises(ValueError):
        splitter.feed('select 1;')


def test_splitstream_bytes_chunks():
    sql = 'select * from foo; select 2;'
    chunks = [sql[:10].encode('utf-8'), sql[10:].encode('utf-8')]
    assert list(sqlparse.splitstream(chunks)) == sqlparse.split(sql)


def test_splitstream_bad_chunk_type():
    splitter = StreamStatementSplitter()
    with pytest.raises(TypeError):
        splitter.feed(42)


def test_splitstream_strip_semicolon():
    splitter = StreamStatementSplitter(strip_semicolon=True)
    assert splitter.feed('select 1; ') == []
    result = splitter.close()
    assert result.statement == 'select 1'
