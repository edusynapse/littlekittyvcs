package blob

import "testing"

func TestPlanParts(t *testing.T) {
	cases := []struct {
		size  int64
		want  []part
		fails bool
	}{
		{
			size: (16 << 20) + 1,
			want: []part{{Number: 1, Size: PartSize}, {Number: 2, Size: 1}},
		},
		{
			size: 20 << 20,
			want: []part{{Number: 1, Size: PartSize}, {Number: 2, Size: 4 << 20}},
		},
		{
			size: 48 << 20,
			want: []part{
				{Number: 1, Size: PartSize},
				{Number: 2, Size: PartSize},
				{Number: 3, Size: PartSize},
			},
		},
		{size: (160 << 30) + 1, fails: true},
	}
	for _, tc := range cases {
		got, err := planParts(tc.size)
		if tc.fails {
			if err == nil {
				t.Fatalf("size %d: expected error", tc.size)
			}
			continue
		}
		if err != nil {
			t.Fatalf("size %d: %v", tc.size, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("size %d: got %#v", tc.size, got)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("size %d: part %d got %#v want %#v", tc.size, i, got[i], tc.want[i])
			}
			if i < len(got)-1 && got[i].Size != PartSize {
				t.Fatalf("size %d: non-final part %d size %d", tc.size, i, got[i].Size)
			}
			if got[i].Number != int32(i+1) {
				t.Fatalf("size %d: part number %d", tc.size, got[i].Number)
			}
		}
	}
}
