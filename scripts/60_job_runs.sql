-- Job execution ownership.
--
-- Every worker execution of a job claims a unique run id. All progress and
-- status writes are conditional on it, so a cancelled or restarted execution
-- can never overwrite the state of the execution that replaced it.
--
-- run_id           : run id of the execution that currently owns the job, NULL
--                    when no execution owns it (queued, or finished).
-- cancel_requested : set by the operator cancel endpoint, cleared when a new
--                    execution claims the job.

ALTER TABLE jobs ADD COLUMN IF NOT EXISTS run_id varchar(128);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cancel_requested boolean NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS jobs_run_id_index ON jobs (run_id);
