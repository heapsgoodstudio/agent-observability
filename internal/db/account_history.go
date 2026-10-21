package db

import (
	"database/sql"
	"errors"
	"math"
	"net/url"
	"strings"
	"time"
)

const AccountHistoryRetention = 90 * 24 * time.Hour
const AccountHistoryLimit = 150000

// AccountKey namespaces providers and keeps unresolved identity distinct.
func AccountKey(provider, account, profile string) string {
	if account == "" {
		return provider + ":profile:" + url.QueryEscape(profile)
	}
	return provider + ":" + url.QueryEscape(account)
}
func AllowanceWindowID(bucket, kind string) string {
	return url.QueryEscape(bucket) + ":" + url.QueryEscape(kind)
}

type AllowanceObservation struct {
	ResetKnown                                    bool
	Provider, AccountKey, WindowID, Label, Source string
	ObservedAt, ResetAt                           int64
	UsedPercent                                   float64
}

// PutAllowanceObservations runs only in the collection path. Repeated source
// snapshots are idempotent and multiple profiles never duplicate an account.
func PutAllowanceObservations(d *sql.DB, provider, profile, accountKey string, observedAt, now int64, rows []AllowanceObservation) error {
	if d == nil {
		return errors.New("database unavailable")
	}
	if provider == "" || profile == "" || !strings.HasPrefix(accountKey, provider+":") || observedAt <= 0 || observedAt > now+60 {
		return errors.New("invalid allowance observation identity or time")
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM allowance_observations WHERE observed_at < ?`, now-int64(AccountHistoryRetention/time.Second)); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM profile_account_observations WHERE observed_at < ?`, now-int64(AccountHistoryRetention/time.Second)); err != nil {
		return err
	}
	if observedAt < now-int64(AccountHistoryRetention/time.Second) {
		return tx.Commit()
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO profile_account_observations(provider,profile,account_key,observed_at) VALUES (?,?,?,?)`, provider, profile, accountKey, observedAt); err != nil {
		return err
	}
	for _, r := range rows {
		if r.WindowID == "" || math.IsNaN(r.UsedPercent) || math.IsInf(r.UsedPercent, 0) {
			continue
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO allowance_observations(provider,account_key,window_id,label,source,observed_at,reset_at,reset_known,used_percent) VALUES (?,?,?,?,?,?,?,?,?)`, provider, accountKey, r.WindowID, r.Label, r.Source, observedAt, r.ResetAt, r.ResetKnown || r.ResetAt != 0, r.UsedPercent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ListAllowanceObservations(d *sql.DB, provider, accountKey string, from, to int64) ([]AllowanceObservation, bool, error) {
	if d == nil {
		return nil, false, errors.New("database unavailable")
	}
	if provider == "" || accountKey == "" {
		return nil, false, errors.New("account history requires an exact scope")
	}
	rows, err := d.Query(`SELECT provider,account_key,window_id,label,source,observed_at,reset_at,reset_known,used_percent FROM allowance_observations WHERE provider=? AND account_key=? AND observed_at>=? AND observed_at<=? ORDER BY observed_at DESC,window_id LIMIT ?`, provider, accountKey, from, to, AccountHistoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AllowanceObservation
	for rows.Next() {
		var r AllowanceObservation
		if err := rows.Scan(&r.Provider, &r.AccountKey, &r.WindowID, &r.Label, &r.Source, &r.ObservedAt, &r.ResetAt, &r.ResetKnown, &r.UsedPercent); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > AccountHistoryLimit
	if truncated {
		out = out[:AccountHistoryLimit]
	}
	// The bounded query keeps the newest observations, readers receive time order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, truncated, nil
}
