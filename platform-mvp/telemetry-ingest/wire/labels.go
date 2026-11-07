package wire

import "sort"

type Label struct{ Name, Value string }

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

func fnvString(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

// HashLabels identifies a label set. The input must already be sorted by
// name. Separator bytes keep ("ab","c") and ("a","bc") apart.
func HashLabels(sorted []Label) uint64 {
	h := uint64(fnvOffset)
	for _, l := range sorted {
		h = fnvString(h, l.Name)
		h ^= 0xff
		h *= fnvPrime
		h = fnvString(h, l.Value)
		h ^= 0xfe
		h *= fnvPrime
	}
	return h
}

func sortLabels(ls []Label) {
	sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
}
