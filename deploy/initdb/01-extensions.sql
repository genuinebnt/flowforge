-- Extensions required by spec 10 (pg_trgm, citext) and, when the image
-- provides it, pgvector for the similarity adapter (spec 22).
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS citext;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector') THEN
    CREATE EXTENSION IF NOT EXISTS vector;
  END IF;
END
$$;

-- Integration tests create throwaway databases from this template so every
-- test database already has the extensions.
\connect template1
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS citext;
