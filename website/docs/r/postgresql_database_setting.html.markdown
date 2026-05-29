---
layout: "postgresql"
page_title: "PostgreSQL: postgresql_database_setting"
sidebar_current: "docs-postgresql-resource-postgresql_database_setting"
description: |-
  Manages a per-database configuration parameter via ALTER DATABASE ... SET ...
---

# postgresql_database_setting

The `postgresql_database_setting` resource manages a single per-database configuration parameter, i.e. a row in the system catalog [`pg_db_role_setting`](https://www.postgresql.org/docs/current/catalog-pg-db-role-setting.html). It corresponds to the SQL statement:

```sql
ALTER DATABASE <database> SET <parameter> = '<value>';
```

Unlike the global `assume_role` attribute on `postgresql_role` (which translates to `ALTER ROLE … SET role = …` and applies cluster-wide), this resource scopes the setting to a specific database. Common use cases:

- Auto-switching a developer role into a project-owned group role on connect: `parameter = "role"`, `value = "<project>_db_owner"`.
- Setting a project-specific `search_path` for a shared role.
- Tuning per-database `statement_timeout` for a service account.

## Example: per-database `assume role` for an Entra ID developer

```hcl
resource "postgresql_role" "owner" {
  name  = "app_db_owner"
  login = false
}

resource "postgresql_role" "dev" {
  name    = "alice@example.com"
  login   = true
  inherit = true
  roles   = [postgresql_role.owner.name]
}

resource "postgresql_database" "db" {
  name  = "app_db"
  owner = postgresql_role.owner.name
}

resource "postgresql_database_setting" "dev_assume_owner" {
  database  = postgresql_database.db.name
  parameter = "role"
  value     = postgresql_role.owner.name
}
```

When anyone connects to `app_db`, their `current_role` is automatically set to `app_db_owner`, so any objects created by ad-hoc DDL or migrations are owned by the group role instead of the individual user.

## Example: setting multiple parameters on the same (database) pair

You can declare independent resources for each parameter; they coexist in the same `pg_db_role_setting` row without conflicting:

```hcl
resource "postgresql_database_setting" "search_path" {
  database  = postgresql_database.db.name
  parameter = "search_path"
  value     = "app, public"
}

resource "postgresql_database_setting" "statement_timeout" {
  database  = postgresql_database.db.name
  parameter = "statement_timeout"
  value     = "5min"
}
```

Concurrent operations against the same `(database)` pair are serialized internally with a transactional advisory lock, so parallel `terraform apply` of multiple resources on the same pair is safe.

## Argument Reference

- `database` - (Required) The database in which the setting applies. Forces a new resource on change.
- `parameter` - (Required) The configuration parameter (any GUC name accepted by `ALTER ROLE`, e.g. `role`, `search_path`, `statement_timeout`). Forces a new resource on change.
- `value` - (Required) The value to assign to the parameter for this `(database)` pair. The provider quotes the value as a string literal; PostgreSQL will interpret and canonicalize it according to the parameter's type.

## Import

Existing settings can be imported using the composite ID `<database>:<parameter>`:

```shell
terraform import postgresql_database_setting.dev_assume_owner \
  'app_db:role'
```

Names with `@`, `.`, or mixed case are supported as-is (the provider quotes identifiers correctly when emitting `ALTER ROLE`). PostgreSQL also allows literal `:` and `\` in quoted identifiers; in the import ID they must be backslash-escaped (`\:` and `\\`) so the three components remain unambiguous. The same encoding is what the resource emits for the state ID, so you can copy a state ID directly:

```shell
# database = "app:blue", parameter = "role"
terraform import postgresql_database_setting.dev_assume_owner \
  'app\:blue:role'
```

Use single quotes (or double-escape) when invoking `terraform import` from a shell, otherwise the shell will eat the backslashes.
