-- Local development: the LOGIN user the api and worker connect as. It gets its
-- privileges only through membership in app_runtime (created by migrations).
CREATE ROLE core_app LOGIN PASSWORD 'dev_app_password';
GRANT CONNECT ON DATABASE core TO core_app;
