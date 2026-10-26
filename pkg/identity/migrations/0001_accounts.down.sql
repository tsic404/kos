-- Roll back the initial identity schema. Dropping the accounts table also
-- removes their sessions and preferences through the foreign keys.

DROP INDEX audit_occurred_at;
DROP TABLE audit;
DROP TABLE preferences;
DROP INDEX sessions_user;
DROP TABLE sessions;
DROP TABLE login_attempts;
DROP INDEX users_role;
DROP TABLE users;
