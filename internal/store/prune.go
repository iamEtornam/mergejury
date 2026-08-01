package store

import "fmt"

// PruneRuns deletes runs older than the given number of days for one repo.
func (s *Store) PruneRuns(repo string, olderThanDays int) (int, error) {
	q := fmt.Sprintf(
		"DELETE FROM runs WHERE repo = '%s' AND started_at < datetime('now', '-%d days')",
		repo, olderThanDays)
	res, _ := s.db.Exec(q)
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RunIDsForRepo lists stored run ids for a repo, newest first.
func (s *Store) RunIDsForRepo(repo string, limit int) ([]int64, error) {
	rows, err := s.db.Query("SELECT id FROM runs WHERE repo = ? ORDER BY id DESC LIMIT ?", repo, limit)
	if err != nil {
		return nil, err
	}
	var out []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out, nil
}
