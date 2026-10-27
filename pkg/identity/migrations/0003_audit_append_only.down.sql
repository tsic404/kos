DROP TRIGGER audit_no_delete;
DROP TRIGGER audit_no_update;

ALTER TABLE audit DROP COLUMN extension;
