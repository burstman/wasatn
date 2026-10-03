-- Server-side session store for alexedwards/scs.
--
-- scs v2.9 removed signed cookies: the cookie now holds an opaque token and the
-- session data lives in this table. Storing sessions in Postgres (rather than the
-- library's default in-memory store) keeps sessions valid across restarts and
-- across multiple web instances, and avoids adding a Redis dependency.
--
-- Table shape matches the pgxstore module: github.com/alexedwards/scs/pgxstore
CREATE TABLE sessions (
    token  text PRIMARY KEY,
    data   bytea NOT NULL,
    expiry timestamptz NOT NULL
);

CREATE INDEX sessions_expiry_idx ON sessions (expiry);