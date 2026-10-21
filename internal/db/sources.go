package db

import "database/sql"

type SessionSource struct{ ID, Path, Cwd, Format string }

func PutSessionSource(d *sql.DB, s SessionSource) error {
	_, err := d.Exec(`INSERT INTO session_sources(id,path,cwd,format) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET path=excluded.path,cwd=excluded.cwd,format=excluded.format`, s.ID, s.Path, s.Cwd, s.Format)
	return err
}
func GetSessionSource(d *sql.DB, id string) (SessionSource, bool, error) {
	s := SessionSource{ID: id}
	err := d.QueryRow(`SELECT path,cwd,format FROM session_sources WHERE id=?`, id).Scan(&s.Path, &s.Cwd, &s.Format)
	if err == sql.ErrNoRows {
		return s, false, nil
	}
	return s, err == nil, err
}
