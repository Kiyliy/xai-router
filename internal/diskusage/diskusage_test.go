package diskusage

import "testing"

func TestOf(t *testing.T) {
	u, err := Of(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u.Total == 0 || u.Free > u.Total || u.UsedPercent() < 0 || u.UsedPercent() > 100 {
		t.Fatalf("usage %+v (%.1f%%)", u, u.UsedPercent())
	}
}
