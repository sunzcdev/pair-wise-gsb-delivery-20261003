from peewee import *
from peewee import sort_models

from .base import BaseTestCase
from .base import ModelTestCase
from .base import TestModel
from .base import db
from .base import requires_models


class CFParent(TestModel):
    a = IntegerField()
    b = CharField()
    class Meta:
        primary_key = CompositeKey('a', 'b')

class CFChild(TestModel):
    parent = CompositeForeignKeyField(CFParent, backref='children',
                                      on_delete='CASCADE')
    name = CharField()

class CFChildNullable(TestModel):
    parent = CompositeForeignKeyField(CFParent, backref='nchildren',
                                      null=True)
    name = CharField()

class CFTeam(TestModel):
    t1 = IntegerField()
    t2 = IntegerField()
    class Meta:
        primary_key = CompositeKey('t1', 't2')

class CFPlayer(TestModel):
    p1 = IntegerField()
    p2 = IntegerField()
    team = CompositeForeignKeyField(CFTeam, backref='players')
    class Meta:
        primary_key = CompositeKey('p1', 'p2')

class CFFan(TestModel):
    player = CompositeForeignKeyField(CFPlayer, backref='fans')

class CFNode(TestModel):
    k1 = IntegerField()
    k2 = IntegerField()
    parent = CompositeForeignKeyField('self', fields=('k1', 'k2'), null=True,
                                      backref='children')
    class Meta:
        primary_key = CompositeKey('k1', 'k2')


class TestCompositeForeignKeyDDL(BaseTestCase):
    def test_create_table_constraint(self):
        sql, _ = CFChild._schema._create_table().query()
        self.assertIn(
            'FOREIGN KEY ("parent_a", "parent_b") '
            'REFERENCES "cf_parent" ("a", "b") ON DELETE CASCADE', sql)
        # Component columns mirror the referenced column types.
        self.assertIn('"parent_a" INTEGER NOT NULL', sql)
        self.assertIn('"parent_b" VARCHAR(255) NOT NULL', sql)

    def test_component_fields_created(self):
        self.assertEqual(CFChild._meta.sorted_field_names,
                         ['id', 'name', 'parent_a', 'parent_b'])
        self.assertEqual(list(CFChild._meta.composite_fks), ['parent'])
        fk = CFChild._meta.composite_fks['parent']
        self.assertEqual(fk.field_names, ('parent_a', 'parent_b'))
        self.assertEqual([f.name for f in fk.rel_fields], ['a', 'b'])
        # The same ordered column set is exposed for joins/access/DDL.
        self.assertEqual([f.name for f in fk.fields],
                         ['parent_a', 'parent_b'])

    def test_index_created_on_component_columns(self):
        indexes = CFChild._schema._create_indexes()
        sql = ' '.join(idx.query()[0] for idx in indexes)
        self.assertIn('("parent_a", "parent_b")', sql)

    def test_match_simple_ddl(self):
        class P(TestModel):
            a = IntegerField()
            b = IntegerField()
            class Meta:
                primary_key = CompositeKey('a', 'b')
        class M(TestModel):
            parent = CompositeForeignKeyField(P, match='SIMPLE')
        sql, _ = M._schema._create_table().query()
        self.assertIn('MATCH SIMPLE', sql)

    def test_match_full_unsupported_databases(self):
        # SQLite parses but does not enforce MATCH FULL, and MySQL ignores
        # the MATCH clause entirely: refuse to emit a wrong constraint.
        for test_db in (SqliteDatabase(':memory:'),
                        MySQLDatabase('test_cfk', autoconnect=False)):
            class P(Model):
                a = IntegerField()
                b = IntegerField()
                class Meta:
                    database = test_db
                    primary_key = CompositeKey('a', 'b')
            class M(Model):
                parent = CompositeForeignKeyField(P, match='FULL')
                class Meta:
                    database = test_db
            self.assertRaises(NotSupportedError, M._schema._create_table)
            self.assertRaises(
                NotSupportedError,
                M._schema._create_foreign_key,
                M._meta.composite_fks['parent'])

    def test_match_full_postgresql(self):
        pg_db = PostgresqlDatabase('test_cfk', autoconnect=False)
        class PGP(Model):
            a = IntegerField()
            b = IntegerField()
            class Meta:
                database = pg_db
                primary_key = CompositeKey('a', 'b')
        class PGC(Model):
            parent = CompositeForeignKeyField(PGP, match='FULL')
            class Meta:
                database = pg_db
        sql, _ = PGC._schema._create_table().query()
        self.assertIn(
            'FOREIGN KEY ("parent_a", "parent_b") REFERENCES "pgp" ("a", "b")'
            ' MATCH FULL', sql)

    def test_deferred_constraint(self):
        class DP(TestModel):
            a = IntegerField()
            b = IntegerField()
            class Meta:
                primary_key = CompositeKey('a', 'b')
        class DC(TestModel):
            parent = CompositeForeignKeyField(
                DP, _deferred=True, deferrable='INITIALLY DEFERRED')
        sql, _ = DC._schema._create_table().query()
        self.assertNotIn('FOREIGN KEY', sql)

        fk = DC._meta.composite_fks['parent']
        sql, _ = DC._schema._create_foreign_key(fk).query()
        self.assertEqual(
            sql,
            'ALTER TABLE "dc" ADD CONSTRAINT "fk_dc_parent_refs_dp" '
            'FOREIGN KEY ("parent_a", "parent_b") REFERENCES "dp" ("a", "b") '
            'DEFERRABLE INITIALLY DEFERRED')

    def test_configuration_errors(self):
        # Deferred model references (other than "self") are not supported.
        self.assertRaises(ImproperlyConfigured,
                          CompositeForeignKeyField, 'NotDefinedYet')

        # Invalid match rule.
        self.assertRaises(ValueError,
                          CompositeForeignKeyField, CFParent, match='PARTIAL')

        # Fewer than two referenced columns.
        def make_single():
            class M(TestModel):
                parent = CompositeForeignKeyField(CFParent, fields=('a',))
        self.assertRaises(ValueError, make_single)

        # Unknown referenced field.
        def make_unknown():
            class M(TestModel):
                parent = CompositeForeignKeyField(CFParent,
                                                  fields=('a', 'missing'))
        self.assertRaises(ImproperlyConfigured, make_unknown)

        # Related model has no composite primary key: fields= is required.
        class Simple(TestModel):
            v = IntegerField()
        def make_no_fields():
            class M(TestModel):
                parent = CompositeForeignKeyField(Simple)
        self.assertRaises(ImproperlyConfigured, make_no_fields)

    def test_sort_models(self):
        self.assertEqual(
            sort_models([CFFan, CFPlayer, CFTeam]),
            [CFTeam, CFPlayer, CFFan])


class TestCompositeForeignKeyRelations(ModelTestCase):
    requires = [CFParent, CFChild, CFChildNullable]

    def setUp(self):
        super(TestCompositeForeignKeyRelations, self).setUp()
        self.execute('PRAGMA foreign_keys=ON')
        self.p1 = CFParent.create(a=1, b='p1')
        self.p2 = CFParent.create(a=2, b='p2')

    def test_create_and_access(self):
        c = CFChild.create(parent=self.p1, name='c1')
        self.assertEqual(c.parent_id, (1, 'p1'))
        self.assertEqual(c.parent.a, 1)
        self.assertEqual(c.parent.b, 'p1')

        # Assignment by tuple of key values.
        c2 = CFChild(name='c2')
        c2.parent = (2, 'p2')
        c2.save()
        self.assertEqual(c2.parent_id, (2, 'p2'))
        self.assertEqual(c2.parent.b, 'p2')

        # Assignment via the object-id accessor.
        c3 = CFChild(name='c3')
        c3.parent_id = (1, 'p1')
        self.assertEqual(c3.parent.a, 1)

        # Invalid assignment raises.
        self.assertRaises(ValueError, setattr, c3, 'parent', (1,))
        self.assertRaises(TypeError, setattr, c3, 'parent', 1)

    def test_unsaved_relation_populated_on_save(self):
        # A related object present only in the relation cache has its key
        # values copied into the component columns on save.
        child = CFChild(name='c')
        child.__rel__['parent'] = self.p1
        child.save(force_insert=True)
        self.assertEqual(child.parent_id, (1, 'p1'))
        self.assertEqual(CFChild.get_by_id(child.id).parent_id, (1, 'p1'))

    def test_join_sql_and_results(self):
        CFChild.create(parent=self.p1, name='c1')
        CFChild.create(parent=self.p2, name='c2')
        query = CFChild.select().join(CFParent).where(CFParent.a == 1)
        sql, _ = query.sql()
        self.assertIn(
            'INNER JOIN "cf_parent" AS "t2" ON '
            '(("t1"."parent_a" = "t2"."a") AND ("t1"."parent_b" = "t2"."b"))',
            sql)
        self.assertEqual([c.name for c in query], ['c1'])

        # Backref join uses the same ordered column set.
        query = CFParent.select().join(CFChild).where(CFChild.name == 'c2')
        sql, _ = query.sql()
        self.assertIn(
            'ON (("t2"."parent_a" = "t1"."a") AND '
            '("t2"."parent_b" = "t1"."b"))', sql)
        self.assertEqual([p.a for p in query], [2])

    def test_join_object_graph(self):
        CFChild.create(parent=self.p1, name='c1')
        query = (CFChild.select(CFChild, CFParent)
                 .join(CFParent)
                 .where(CFChild.name == 'c1'))
        with self.assertQueryCount(1):
            child = query.get()
            self.assertEqual(child.parent.a, 1)
            self.assertEqual(child.parent.b, 'p1')

    def test_left_outer_join_null_fk(self):
        CFChildNullable.create(name='cn')
        query = (CFChildNullable.select(CFChildNullable, CFParent)
                 .join(CFParent, JOIN.LEFT_OUTER))
        child = query.get()
        self.assertIsNone(child.parent)
        self.assertEqual(child.parent_id, (None, None))

    def test_backref(self):
        CFChild.create(parent=self.p1, name='c1')
        CFChild.create(parent=self.p1, name='c2')
        CFChild.create(parent=self.p2, name='c3')
        self.assertEqual(
            sorted(c.name for c in self.p1.children), ['c1', 'c2'])

    def test_equality_filter(self):
        CFChild.create(parent=self.p1, name='c1')
        CFChild.create(parent=self.p2, name='c2')
        query = CFChild.select().where(CFChild.parent == self.p2)
        sql, params = query.sql()
        self.assertIn('"parent_a" = ?', sql)
        self.assertIn('"parent_b" = ?', sql)
        self.assertEqual(params, [2, 'p2'])
        self.assertEqual([c.name for c in query], ['c2'])

        # Tuple comparison and !=.
        query = CFChild.select().where(CFChild.parent != (2, 'p2'))
        self.assertEqual([c.name for c in query], ['c1'])

    def test_foreign_key_enforced(self):
        self.assertRaises(IntegrityError, CFChild.create,
                          parent=(9, 'missing'), name='bad')

    def test_partial_null_match_simple(self):
        # MATCH SIMPLE (the default): a key with any NULL component is not
        # checked and cannot reference a row.
        c = CFChildNullable.create(name='cn', parent_a=1, parent_b=None)
        self.assertEqual(c.parent_id, (1, None))
        self.assertIsNone(c.parent)

        # The partial key does not join to anything, even though a parent
        # with a = 1 exists.
        query = (CFChildNullable.select()
                 .join(CFParent)
                 .where(CFChildNullable.name == 'cn'))
        self.assertEqual(list(query), [])

        # All-NULL key also resolves to None.
        c2 = CFChildNullable.create(name='cn2')
        self.assertIsNone(c2.parent)

    def test_on_delete_cascade(self):
        CFChild.create(parent=self.p1, name='c1')
        CFChild.create(parent=self.p2, name='c2')
        self.p1.delete_instance()
        self.assertEqual([c.name for c in CFChild.select()], ['c2'])

    def test_recursive_delete(self):
        CFChild.create(parent=self.p1, name='c1')
        CFChild.create(parent=self.p2, name='c2')
        CFChildNullable.create(parent=self.p1, name='n1')
        self.p1.delete_instance(recursive=True)
        self.assertEqual([c.name for c in CFChild.select()], ['c2'])
        # Nullable children are nulled-out by default, not deleted.
        child = CFChildNullable.get()
        self.assertEqual(child.parent_id, (None, None))

    def test_recursive_delete_nullable(self):
        CFChildNullable.create(parent=self.p1, name='n1')
        self.p1.delete_instance(recursive=True, delete_nullable=True)
        self.assertEqual(CFChildNullable.select().count(), 0)


class TestCompositeForeignKeyTransitiveDelete(ModelTestCase):
    requires = [CFTeam, CFPlayer, CFFan]

    def test_transitive_dependencies(self):
        team = CFTeam.create(t1=1, t2=1)
        player = CFPlayer.create(p1=1, p2=1, team=team)
        CFFan.create(player=player)
        CFFan.create(player=player)

        # The grandchild predicate is a row-value IN subquery comparing the
        # full ordered column set.
        exprs = dict((fk.model, node) for node, fk in team.dependencies())
        sql, params = CFFan.delete().where(exprs[CFFan]).sql()
        self.assertIn(
            '("cf_fan"."player_p1", "cf_fan"."player_p2") IN '
            '(SELECT "t1"."p1", "t1"."p2" FROM "cf_player" AS "t1" WHERE '
            '(("t1"."team_t1" = ?) AND ("t1"."team_t2" = ?)))', sql)
        sql, params = CFPlayer.delete().where(exprs[CFPlayer]).sql()
        self.assertIn('"team_t1" = ?', sql)
        self.assertIn('"team_t2" = ?', sql)

        team.delete_instance(recursive=True)
        self.assertEqual(CFFan.select().count(), 0)
        self.assertEqual(CFPlayer.select().count(), 0)
        self.assertEqual(CFTeam.select().count(), 0)


class TestCompositeForeignKeySelfReferential(ModelTestCase):
    requires = [CFNode]

    def test_self_referential(self):
        n1 = CFNode.create(k1=1, k2=1)
        n2 = CFNode.create(k1=2, k2=2, parent=n1)
        CFNode.create(k1=3, k2=3)

        self.assertEqual(n2.parent.k1, 1)
        self.assertEqual([n.k1 for n in n1.children], [2])

        query = CFNode.select().join(
            CFNode.alias(), on=CFNode.parent).where(CFNode.k1 == 2)
        self.assertEqual([n.k1 for n in query], [2])


class TestCompositeForeignKeyExplicitFields(ModelTestCase):
    def test_explicit_fields(self):
        class Holder(TestModel):
            h1 = CharField()
            h2 = CharField()
            class Meta:
                indexes = ((('h1', 'h2'), True),)

        class Card(TestModel):
            holder = CompositeForeignKeyField(Holder, fields=('h1', 'h2'),
                                              backref='cards', null=True)

        with self.database.bind_ctx([Holder, Card], False, False):
            self.database.create_tables([Holder, Card])
            try:
                h = Holder.create(h1='x', h2='y')
                c = Card.create(holder=h)
                self.assertEqual(c.holder_id, ('x', 'y'))
                self.assertEqual(c.holder.h2, 'y')
                self.assertEqual([cc.id for cc in h.cards], [c.id])
            finally:
                self.database.drop_tables([Holder, Card])


class TestCompositeForeignKeyUnsupported(ModelTestCase):
    requires = [CFParent, CFChild]

    def test_prefetch_not_supported(self):
        from peewee import prefetch
        self.assertRaises(ImproperlyConfigured, prefetch,
                          CFParent.select(), CFChild.select())
        self.assertRaises(ImproperlyConfigured, prefetch,
                          CFChild.select(), CFParent.select())

    def test_load_not_supported(self):
        self.assertRaises(ValueError, Load, CFChild.parent)


class TestCompositeForeignKeyQueryHelpers(ModelTestCase):
    requires = [CFParent, CFChild]

    def setUp(self):
        super(TestCompositeForeignKeyQueryHelpers, self).setUp()
        self.p1 = CFParent.create(a=1, b='p1')
        self.p2 = CFParent.create(a=2, b='p2')
        CFChild.create(parent=self.p1, name='c1')
        CFChild.create(parent=self.p2, name='c2')

    def test_insert_and_update_by_name(self):
        # The composite field (and its object-id name) may be used directly.
        CFChild.insert(parent=(1, 'p1'), name='c3').execute()
        CFChild.insert(parent_id=(2, 'p2'), name='c4').execute()
        self.assertEqual(
            [(c.name, c.parent_id) for c in CFChild.select().order_by(CFChild.name)],
            [('c1', (1, 'p1')), ('c2', (2, 'p2')),
             ('c3', (1, 'p1')), ('c4', (2, 'p2'))])

        (CFChild.update(parent=(1, 'p1'))
         .where(CFChild.name == 'c4')
         .execute())
        self.assertEqual(CFChild.get(CFChild.name == 'c4').parent_id,
                         (1, 'p1'))

        # The dict form is equivalent.
        CFChild.insert({'parent': (2, 'p2'), 'name': 'c5'}).execute()
        self.assertEqual(CFChild.get(CFChild.name == 'c5').parent_id,
                         (2, 'p2'))

    def test_filter_traversal(self):
        query = CFChild.select().filter(parent__b='p1')
        self.assertEqual([c.name for c in query], ['c1'])
        query = CFParent.select().filter(children__name='c2')
        self.assertEqual([p.b for p in query], ['p2'])

    def test_subclass_indexes_its_own_columns(self):
        class SubParent(TestModel):
            a = IntegerField()
            b = IntegerField()
            class Meta:
                primary_key = CompositeKey('a', 'b')

        class SubChild(TestModel):
            parent = CompositeForeignKeyField(SubParent, backref='kids')

        class SubChild2(SubChild):
            extra = IntegerField(default=0)

        for model, table in ((SubChild, 'sub_child'),
                             (SubChild2, 'sub_child2')):
            indexes = [q.query()[0]
                       for q in model._schema._create_indexes()]
            self.assertTrue(any(
                'ON "%s" ("parent_a", "parent_b")' % table in sql
                for sql in indexes), (table, indexes))

    def test_migration_build_uses_composite_field(self):
        from playhouse.migrations import _build_model
        todos = []
        lines = _build_model(CFChild, set(), todos, {CFParent: 'CFParent'})
        sql = '\n'.join(lines)
        # The auto-created component columns are re-created by the composite
        # field, not as bogus _CompositeFKComponentField declarations.
        self.assertNotIn('_CompositeFKComponentField', sql)
        self.assertIn(
            "parent = CompositeForeignKeyField(CFParent, fields=('a', 'b'), "
            "backref='children', on_delete='CASCADE')", sql)
        self.assertEqual(todos, [])

        # A user-declared component column is preserved.
        class UParent(TestModel):
            a = IntegerField()
            b = IntegerField()
            class Meta:
                primary_key = CompositeKey('a', 'b')
        class UChild(TestModel):
            parent_a = IntegerField(null=True)
            parent = CompositeForeignKeyField(UParent, backref='children')

        sql = '\n'.join(_build_model(UChild, set(), [], {UParent: 'UParent'}))
        self.assertIn('parent_a = IntegerField(null=True)', sql)
        self.assertIn('parent = CompositeForeignKeyField(', sql)

