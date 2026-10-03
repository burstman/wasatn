-- Reverses 0001_init.up.sql.
--
-- Tables are dropped in reverse dependency order, then the enums. The users
-- table is removed last because every other table references it.
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS message_logs;
DROP TABLE IF EXISTS campaigns;
DROP TABLE IF EXISTS templates;
DROP TABLE IF EXISTS contacts;
DROP TABLE IF EXISTS connections;
DROP TABLE IF EXISTS users;

DROP TYPE IF EXISTS message_status;
DROP TYPE IF EXISTS schedule_kind;
DROP TYPE IF EXISTS campaign_status;
DROP TYPE IF EXISTS template_status;
DROP TYPE IF EXISTS template_category;
DROP TYPE IF EXISTS connection_status;
DROP TYPE IF EXISTS user_plan;