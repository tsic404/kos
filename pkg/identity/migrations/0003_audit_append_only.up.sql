-- The audit stream gains the extension identity C-14 requires, and becomes
-- append-only: a record cannot be rewritten or removed, not even by the party
-- it describes. The triggers are the guarantee; the API exposes no write path.

ALTER TABLE audit ADD COLUMN extension TEXT NOT NULL DEFAULT '';

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit records are append-only');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit records are append-only');
END;
