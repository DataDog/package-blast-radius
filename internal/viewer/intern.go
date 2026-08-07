package viewer

// interner maps repeated strings to int32 ids. A report's paths are mostly
// repetition — the same names, ranges like "^1.0.0", and intermediate versions
// recur across millions of hops. Storing an id instead of a string header plus
// bytes is the difference between holding a gigabyte-scale report and not.
type interner struct {
	ids  map[string]int32
	strs []string
}

func newInterner(capacity int) *interner {
	return &interner{ids: make(map[string]int32, capacity)}
}

func (in *interner) id(s string) int32 {
	if id, ok := in.ids[s]; ok {
		return id
	}
	id := int32(len(in.strs))
	in.strs = append(in.strs, s)
	in.ids[s] = id
	return id
}

func (in *interner) str(id int32) string {
	if id < 0 || int(id) >= len(in.strs) {
		return ""
	}
	return in.strs[id]
}

// Always non-nil: a depth-1 route has no intermediate hops, an empty list not a
// missing one.
func (in *interner) strs32(ids []int32) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = in.str(id)
	}
	return out
}

// seal releases the lookup map. Reads only go id -> string, and on the largest
// reports the map outweighs the string table it indexes.
func (in *interner) seal() {
	in.ids = nil
}

func (in *interner) len() int { return len(in.strs) }
