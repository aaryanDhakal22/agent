package models

// Raw is the connection or transaction behind q, for the few hand-written
// statements outside sqlc (repositories/payment_card_repo.go). Not generated:
// sqlc leaves this file alone.
func (q *Queries) Raw() DBTX { return q.db }
