package sqlguard

import "testing"

func TestEffectiveVerb(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":          "SELECT",
		"  select * from t": "SELECT",
		"/* c */ SELECT 1":  "SELECT",
		"WITH cte AS (SELECT 1) SELECT * FROM cte":                                                      "SELECT",
		"WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<5) SELECT i FROM seq": "SELECT",
		"WITH a AS (SELECT 1), b AS (SELECT 2) SELECT * FROM a, b":                                      "SELECT",
		"WITH cte(x,y) AS (SELECT 1,2) SELECT x FROM cte":                                               "SELECT",
		"WITH cte AS NOT MATERIALIZED (SELECT 1) SELECT * FROM cte":                                     "SELECT",
		"WITH cte AS (SELECT id FROM t) INSERT INTO log SELECT id FROM cte":                             "INSERT",
		"WITH cte AS (SELECT 1) UPDATE t SET x=1 WHERE id IN (SELECT * FROM cte)":                       "UPDATE",
		"WITH cte AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM cte)":                          "DELETE",
		"INSERT INTO t VALUES (1)":                                                                      "INSERT",
		"UPDATE t SET x=1":                                                                              "UPDATE",
		"WITH a AS (SELECT ')' AS c) SELECT * FROM a":                                                   "SELECT",
	}
	for q, want := range cases {
		if got := EffectiveVerb(q); got != want {
			t.Errorf("EffectiveVerb(%q) = %q, want %q", q, got, want)
		}
	}
}
