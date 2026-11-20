# Centrum migrations

Centrum is the legacy project: its business tables live in the `public`
schema and were created by the historical platform migration chain in
`internal/db/migrations/`.

This folder is intentionally empty of `.sql` files. New projects put their
schema and tables here, for example:

```text
projects/acme/migrations/0001_init.sql
```

The runner reads `projects/<slug>/migrations/*.sql`, creates the project's
schema from `project.json`, sets `search_path`, and records each version in
`public.project_migrations`.
