-- Refuses while a drain has been signed: those rows are the record of moved funds (and of ledger gas posted).
DO $$
BEGIN
	IF EXISTS (SELECT 1 FROM hot_cutover_drains) THEN
		RAISE EXCEPTION 'cannot downgrade: a Hot cutover has signed drain transactions';
	END IF;
END $$;
DROP TABLE hot_cutover_drains;
DROP TABLE hot_cutover;
