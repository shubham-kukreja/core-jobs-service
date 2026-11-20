#!/bin/sh
# Runs once, when the data volume is first initialised. Creates the LOGIN
# user the api and worker connect as; it gets privileges only through
# app_runtime (the migrations grant DML to that role and reuse it if present).
set -eu
psql -v ON_ERROR_STOP=1 -v app_password="$APP_DB_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE app_runtime NOLOGIN;
CREATE ROLE core_app LOGIN PASSWORD :'app_password';
GRANT app_runtime TO core_app;
GRANT CONNECT ON DATABASE core TO core_app;
SQL
