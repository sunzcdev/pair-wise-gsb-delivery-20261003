
import datetime

class _Migration:
    def __init__(self, name, fn):
        self.name = name
        self.fn = fn

class _Applied:
    def __init__(self, name, applied_at):
        self.name = name
        self.applied_at = applied_at

class LegacyMigrations:
    # Mimics the sqlite-migrate 0.x Migrations class, in particular
    # apply(db, stop_before=None) taking a single string
    migrations_table = "_sqlite_migrations"

    def __init__(self, name):
        self.name = name
        self._migrations = []

    def __call__(self, fn):
        self._migrations.append(_Migration(fn.__name__, fn))
        return fn

    def ensure_migrations_table(self, db):
        db.table(self.migrations_table).create(
            {"migration_set": str, "name": str, "applied_at": str},
            pk=("migration_set", "name"),
            if_not_exists=True,
        )

    def applied(self, db):
        self.ensure_migrations_table(db)
        return [
            _Applied(row["name"], row["applied_at"])
            for row in db.table(self.migrations_table).rows_where(
                "migration_set = ?", [self.name]
            )
        ]

    def pending(self, db):
        applied = {m.name for m in self.applied(db)}
        return [m for m in self._migrations if m.name not in applied]

    def apply(self, db, stop_before=None):
        for migration in self.pending(db):
            if migration.name == stop_before:
                return
            migration.fn(db)
            db.table(self.migrations_table).insert(
                {
                    "migration_set": self.name,
                    "name": migration.name,
                    "applied_at": str(
                        datetime.datetime.now(datetime.timezone.utc)
                    ),
                }
            )

legacy = LegacyMigrations("legacy_set")

@legacy
def first(db):
    db.table("first").insert({"hello": "world"})

@legacy
def second(db):
    db.table("second").insert({"hello": "world"})
