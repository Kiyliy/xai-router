// Package diskusage reports the size and free space of the volume holding
// a path (the data directory), so the console can warn before the disk
// fills up: on a busy gateway the logs, not SQLite's write rate, are the
// limit.
package diskusage

// Usage of one volume in bytes.
type Usage struct {
	Total, Free uint64
}

// UsedPercent is the share of the volume in use (0-100).
func (u Usage) UsedPercent() float64 {
	if u.Total == 0 {
		return 0
	}
	return float64(u.Total-u.Free) * 100 / float64(u.Total)
}

// Of returns the usage of the volume holding path.
func Of(path string) (Usage, error) { return of(path) }
