-- This migration introduces a caching table for message sequence numbers
-- to avoid expensive ROW_NUMBER() window functions on large mailboxes.

-- 1. Create the message_sequences table
-- This table will store a mapping of mailbox, UID, and its calculated sequence number.
CREATE TABLE message_sequences (
    mailbox_id BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    uid BIGINT NOT NULL,
    seqnum INT NOT NULL,
    PRIMARY KEY (mailbox_id, uid),
    -- A mailbox cannot have two messages with the same sequence number.
    UNIQUE (mailbox_id, seqnum)
);

-- 2. Create the trigger function to maintain the cache
CREATE OR REPLACE FUNCTION maintain_message_sequences()
RETURNS TRIGGER AS
$$
DECLARE
    v_mailbox_id BIGINT;
    affected_mailboxes_query TEXT;
BEGIN
    -- This trigger function rebuilds the sequence numbers for affected mailboxes.
    -- We get the mailbox_id from the transition tables, depending on trigger event.
    
    -- Build query based on available transition tables
    IF TG_OP = 'INSERT' THEN
        affected_mailboxes_query := 'SELECT DISTINCT mailbox_id FROM new_table WHERE mailbox_id IS NOT NULL';
    ELSIF TG_OP = 'DELETE' THEN
        affected_mailboxes_query := 'SELECT DISTINCT mailbox_id FROM old_table WHERE mailbox_id IS NOT NULL';
    ELSE -- UPDATE
        affected_mailboxes_query := '
            SELECT DISTINCT mailbox_id FROM new_table WHERE mailbox_id IS NOT NULL
            UNION
            SELECT DISTINCT mailbox_id FROM old_table WHERE mailbox_id IS NOT NULL';
    END IF;
    
    -- Process all affected mailboxes
    FOR v_mailbox_id IN EXECUTE affected_mailboxes_query LOOP
        -- Lock the mailbox to prevent concurrent modifications from other transactions.
        PERFORM pg_advisory_xact_lock(v_mailbox_id);

        -- Atomically rebuild the sequence numbers for the entire mailbox.
        -- This is more robust and often faster for bulk operations than per-row adjustments.
        DELETE FROM message_sequences WHERE mailbox_id = v_mailbox_id;
        INSERT INTO message_sequences (mailbox_id, uid, seqnum)
        SELECT m.mailbox_id, m.uid, ROW_NUMBER() OVER (ORDER BY m.uid)
        FROM messages m
        WHERE m.mailbox_id = v_mailbox_id AND m.expunged_at IS NULL;
    END LOOP;

    RETURN NULL; -- Result is ignored for AFTER STATEMENT triggers.
END;
$$ LANGUAGE plpgsql;

-- 3. Create the triggers on the messages table
-- We need separate triggers for each event type when using transition tables.
CREATE TRIGGER trigger_maintain_message_sequences_insert
AFTER INSERT ON messages
REFERENCING NEW TABLE AS new_table
FOR EACH STATEMENT
EXECUTE FUNCTION maintain_message_sequences();

CREATE TRIGGER trigger_maintain_message_sequences_update
AFTER UPDATE ON messages
REFERENCING OLD TABLE AS old_table NEW TABLE AS new_table
FOR EACH STATEMENT
EXECUTE FUNCTION maintain_message_sequences();

CREATE TRIGGER trigger_maintain_message_sequences_delete
AFTER DELETE ON messages
REFERENCING OLD TABLE AS old_table
FOR EACH STATEMENT
EXECUTE FUNCTION maintain_message_sequences();

-- 4. Populate the message_sequences table for all existing non-expunged messages
-- This is a one-time operation to bootstrap the cache.
INSERT INTO message_sequences (mailbox_id, uid, seqnum)
SELECT
    m.mailbox_id,
    m.uid,
    ROW_NUMBER() OVER (PARTITION BY m.mailbox_id ORDER BY m.uid) AS seqnum
FROM
    messages m
WHERE
    m.expunged_at IS NULL AND m.mailbox_id IS NOT NULL;