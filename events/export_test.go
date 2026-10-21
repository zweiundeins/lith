package events

// ScanAll makes st's Since lookups scan from the compacted position on
// every call, as they did before they kept marks.
func ScanAll(st *Store) { st.scanAll = true }
