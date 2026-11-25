from sqlalchemy import Column
from sqlalchemy import create_engine
from sqlalchemy import event
from sqlalchemy import Integer
from sqlalchemy import MetaData
from sqlalchemy import select
from sqlalchemy import Table
from sqlalchemy import TypeDecorator
from sqlalchemy import type_coerce
from sqlalchemy.testing import fixtures


class ModeValue(TypeDecorator):
    impl = Integer
    cache_ok = True

    processor_factories = 0

    def result_processor(self, dialect, coltype):
        type(self).processor_factories += 1
        return super().result_processor(dialect, coltype)

    def process_execution_result_value(self, value, context):
        mode = context.execution_options.get("fixture_mode", "plain")
        return f"{mode}:{value}"


class ModeFactory(TypeDecorator):
    impl = Integer
    cache_ok = True

    def execution_result_processor(self, context, coltype):
        mode = context.execution_options.get("fixture_mode", "plain")

        def process(value):
            return f"{mode}:{value}"

        return process


def fixture_table(engine):
    metadata = MetaData()
    table = Table(
        "items",
        metadata,
        Column("id", Integer, primary_key=True),
        Column("value", Integer),
    )
    metadata.create_all(engine)
    with engine.begin() as conn:
        conn.execute(
            table.insert(),
            [{"id": 1, "value": 11}, {"id": 2, "value": 22}],
        )
    return table


class TestExecutionResultProcessing(fixtures.TestBase):
    def test_process_execution_result_value_uses_current_options(self):
        ModeValue.processor_factories = 0
        engine = create_engine("sqlite://")
        table = fixture_table(engine)
        stmt = select(
            type_coerce(table.c.value, ModeValue())
        ).where(table.c.id == 1)

        hits = []
        event.listen(
            engine,
            "before_cursor_execute",
            lambda conn, cursor, sql, params, context, many: hits.append(
                context.cache_hit
            ),
        )

        with engine.connect() as conn:
            values = [
                conn.execution_options(fixture_mode=mode).scalar(stmt)
                for mode in ("plain", "alternate", "plain")
            ]

        assert values == ["plain:11", "alternate:11", "plain:11"]
        assert [hit.name for hit in hits] == [
            "CACHE_MISS",
            "CACHE_HIT",
            "CACHE_HIT",
        ]
        assert ModeValue.processor_factories == 1

    def test_execution_result_processor_factory(self):
        engine = create_engine("sqlite://")
        table = fixture_table(engine)
        stmt = select(
            type_coerce(table.c.value, ModeFactory())
        ).where(table.c.id == 1)

        with engine.connect() as conn:
            values = [
                conn.execution_options(fixture_mode=mode).scalar(stmt)
                for mode in ("plain", "alternate", "plain")
            ]

        assert values == ["plain:11", "alternate:11", "plain:11"]

    def test_interleaved_results_keep_their_own_contexts(self, tmp_path):
        engine = create_engine(f"sqlite:///{tmp_path / 'interleaved.db'}")
        table = fixture_table(engine)
        stmt = select(
            type_coerce(table.c.value, ModeValue())
        ).order_by(table.c.id)

        with engine.connect() as first, engine.connect() as second:
            first_result = first.execution_options(
                fixture_mode="plain"
            ).execute(stmt)
            second_result = second.execution_options(
                fixture_mode="alternate"
            ).execute(stmt)

            values = [
                first_result.fetchone()[0],
                second_result.fetchone()[0],
                first_result.fetchone()[0],
                second_result.fetchone()[0],
            ]

        assert values == [
            "plain:11",
            "alternate:11",
            "plain:22",
            "alternate:22",
        ]

    def test_two_connections_share_hot_compiled_cache(self, tmp_path):
        db_path = tmp_path / "execution-result.db"
        engine = create_engine(f"sqlite:///{db_path}")
        table = fixture_table(engine)
        stmt = select(
            type_coerce(table.c.value, ModeValue())
        ).where(table.c.id == 1)

        with engine.connect() as warmup:
            warmup.scalar(stmt)

        cache_entries = len(engine._compiled_cache)
        hits = []
        event.listen(
            engine,
            "before_cursor_execute",
            lambda conn, cursor, sql, params, context, many: hits.append(
                context.cache_hit
            ),
        )

        with engine.connect() as first, engine.connect() as second:
            first_value = first.execution_options(
                fixture_mode="plain"
            ).scalar(stmt)
            second_value = second.execution_options(
                fixture_mode="alternate"
            ).scalar(stmt)
            first_again = first.execution_options(
                fixture_mode="plain"
            ).scalar(stmt)

        assert [first_value, second_value, first_again] == [
            "plain:11",
            "alternate:11",
            "plain:11",
        ]
        assert len(hits) == 3
        assert all(hit.name == "CACHE_HIT" for hit in hits)
        assert len(engine._compiled_cache) == cache_entries
