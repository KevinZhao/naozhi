package session

// keyForID is r.ss.KeyForID's key alone, "" when the ID is not indexed.
func keyForID(r *Router, id string) string {
	k, _ := r.ss.KeyForID(id)
	return k
}
