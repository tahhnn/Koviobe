-- Drop the DB-side default on license_codes.duration_days.
--
-- GORM omits a zero-valued field from an INSERT when the column declares a
-- default, so a code minted with duration_days = 0 (meaning "lifetime") was
-- stored as 30 and granted a 30-day plan instead. The struct tag no longer
-- declares a default, so GORM now writes the 0 explicitly; this drops the
-- column default too, so nothing can reintroduce the behaviour.
--
-- Safe to run on a database that never had the default.

ALTER TABLE license_codes ALTER COLUMN duration_days DROP DEFAULT;

SELECT column_name, column_default, is_nullable
FROM information_schema.columns
WHERE table_name = 'license_codes' AND column_name = 'duration_days';
