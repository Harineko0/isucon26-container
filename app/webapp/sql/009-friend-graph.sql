-- One durable adjacency list per vertex. Post and account state are still read
-- from their base tables in the feed's snapshot. Slots never leave this DB.
BEGIN;
LOCK TABLE friendships IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE IF NOT EXISTS fof_vertices (
    user_id varchar(36) PRIMARY KEY,
    slot bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    -- Paired arrays allow joining open posts by author ID without one dictionary
    -- lookup per two-hop vertex. Inner graph aggregation uses integer slots.
    neighbors bigint[] NOT NULL DEFAULT '{}',
    neighbor_ids varchar(36)[] NOT NULL DEFAULT '{}'
);

CREATE OR REPLACE FUNCTION isureal_refresh_friend_vertices(ids varchar[]) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    -- Lock/create every endpoint in a common order. WHERE false takes the
    -- conflict lock without writing another tuple. IDs may also be endpoints
    -- of a concurrent friendship that did not exist when our statement began.
    INSERT INTO fof_vertices AS v (user_id)
        SELECT id FROM unnest(ids) AS input(id) ORDER BY id
    ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id WHERE false;

    -- This separate VOLATILE-function statement sees any edge committed while
    -- waiting above. Keep one neighbor per incident edge, including self-edges.
    WITH targets AS MATERIALIZED (SELECT unnest(ids) AS id),
    edges AS (
        SELECT f.user_low_id AS id, f.user_high_id AS peer FROM targets t
        JOIN friendships f ON f.user_low_id = t.id
        UNION ALL
        SELECT f.user_high_id, f.user_low_id FROM targets t
        JOIN friendships f ON f.user_high_id = t.id AND f.user_low_id <> t.id
    ), adjacent AS (
        SELECT e.id, array_agg(p.slot ORDER BY p.slot) AS slots,
               array_agg(p.user_id ORDER BY p.slot) AS names
        FROM edges e JOIN fof_vertices p ON p.user_id = e.peer GROUP BY e.id
    )
    UPDATE fof_vertices v SET neighbors = COALESCE(a.slots, '{}'), neighbor_ids = COALESCE(a.names, '{}')
    FROM targets t LEFT JOIN adjacent a ON a.id = t.id WHERE v.user_id = t.id;
END $$;

CREATE OR REPLACE FUNCTION isureal_sync_friend_vertices() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE ids varchar[];
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        TRUNCATE fof_vertices RESTART IDENTITY;
        RETURN NULL;
    ELSIF TG_OP = 'UPDATE' THEN
        SELECT ARRAY(SELECT user_low_id FROM old_edges UNION SELECT user_high_id FROM old_edges
                     UNION SELECT user_low_id FROM new_edges UNION SELECT user_high_id FROM new_edges) INTO ids;
    ELSE
        -- INSERT and DELETE each name their transition table changed_edges.
        SELECT ARRAY(SELECT user_low_id FROM changed_edges UNION SELECT user_high_id FROM changed_edges) INTO ids;
    END IF;
    IF cardinality(ids) > 0 THEN
        PERFORM isureal_refresh_friend_vertices(ids);
    END IF;
    RETURN NULL;
END $$;

-- On initial installation (including an existing populated DB), backfill before
-- exposing the projection. Later initializes rebuild in the same transaction as
-- the TRUNCATE + bulk restore in reset.sql, even with replication_role=replica.
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_trigger WHERE tgrelid = 'friendships'::regclass AND tgname = 'fof_edges_insert') THEN
        TRUNCATE fof_vertices RESTART IDENTITY;
        PERFORM isureal_refresh_friend_vertices(ARRAY(
            SELECT user_low_id FROM friendships UNION SELECT user_high_id FROM friendships));
    END IF;
END $$;

CREATE OR REPLACE TRIGGER fof_edges_insert AFTER INSERT ON friendships
REFERENCING NEW TABLE AS changed_edges FOR EACH STATEMENT EXECUTE FUNCTION isureal_sync_friend_vertices();
CREATE OR REPLACE TRIGGER fof_edges_delete AFTER DELETE ON friendships
REFERENCING OLD TABLE AS changed_edges FOR EACH STATEMENT EXECUTE FUNCTION isureal_sync_friend_vertices();
CREATE OR REPLACE TRIGGER fof_edges_update AFTER UPDATE ON friendships
REFERENCING NEW TABLE AS new_edges OLD TABLE AS old_edges FOR EACH STATEMENT EXECUTE FUNCTION isureal_sync_friend_vertices();
CREATE OR REPLACE TRIGGER fof_edges_truncate AFTER TRUNCATE ON friendships
FOR EACH STATEMENT EXECUTE FUNCTION isureal_sync_friend_vertices();
ALTER TABLE friendships ENABLE ALWAYS TRIGGER fof_edges_insert;
ALTER TABLE friendships ENABLE ALWAYS TRIGGER fof_edges_delete;
ALTER TABLE friendships ENABLE ALWAYS TRIGGER fof_edges_update;
ALTER TABLE friendships ENABLE ALWAYS TRIGGER fof_edges_truncate;
COMMIT;
