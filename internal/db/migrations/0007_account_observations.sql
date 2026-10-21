-- Prospective observations retain the identity actually reported at that time.
-- An unresolved profile is never later rewritten to its current account.
CREATE TABLE allowance_observations (
  provider TEXT NOT NULL,
  account_key TEXT NOT NULL,
  window_id TEXT NOT NULL,
  label TEXT NOT NULL,
  source TEXT NOT NULL,
  observed_at INTEGER NOT NULL,
  reset_at INTEGER NOT NULL DEFAULT 0,
  reset_known INTEGER NOT NULL DEFAULT 0,
  used_percent REAL NOT NULL,
  PRIMARY KEY(provider, account_key, window_id, observed_at, reset_at, reset_known)
);
CREATE INDEX allowance_observations_time ON allowance_observations(observed_at);
CREATE TABLE profile_account_observations (
  provider TEXT NOT NULL,
  profile TEXT NOT NULL,
  account_key TEXT NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY(provider, profile, observed_at, account_key)
);
CREATE INDEX profile_account_observations_time ON profile_account_observations(observed_at);
