package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// Deletes rows whose session no longer exists. Only reachable because a
// deletion was once done with foreign keys disabled; mole's own store sets
// _pragma=foreign_keys(1) and cascades correctly.
func main() {
	db, err := sql.Open("sqlite", os.Args[1]+"?_pragma=foreign_keys(1)")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	for _, t := range []string{"reservations", "leads", "claims", "tool_calls", "fetch_outcomes", "spans"} {
		res, err := db.Exec(fmt.Sprintf(
			`DELETE FROM %s WHERE session_id IS NOT NULL
			   AND session_id NOT IN (SELECT id FROM sessions)`, t))
		if err != nil {
			fmt.Printf("  %-15s error: %v\n", t, err)
			continue
		}
		n, _ := res.RowsAffected()
		fmt.Printf("  %-15s deleted %d orphan(s)\n", t, n)
	}
}
