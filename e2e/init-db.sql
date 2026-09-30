-- One database per service on the shared Postgres (the postgres image only creates $POSTGRES_DB).
CREATE DATABASE ledger;
CREATE DATABASE chain_watcher;
CREATE DATABASE api_gateway;
