package blob

import "fmt"

const (
	SinglePutMax = 16 << 20 // objects up to this size use one PutObject
	PartSize     = 16 << 20 // multipart part size; S3 and Spaces reject parts under 5 MiB except the last
	MaxParts     = 10000    // S3 limit
)

type part struct {
	Number int32
	Size   int64
}

// planParts splits size into parts of PartSize, with a short last part.
// Part numbers start at 1. size above PartSize*MaxParts is an error.
func planParts(size int64) ([]part, error) {
	if size <= 0 {
		return nil, fmt.Errorf("blob: size %d must be positive", size)
	}
	max := int64(PartSize) * int64(MaxParts)
	if size > max {
		return nil, fmt.Errorf("blob: size %d exceeds maximum %d", size, max)
	}
	parts := make([]part, 0, int((size+int64(PartSize)-1)/int64(PartSize)))
	var number int32 = 1
	for remaining := size; remaining > 0; number++ {
		n := int64(PartSize)
		if remaining < n {
			n = remaining
		}
		parts = append(parts, part{Number: number, Size: n})
		remaining -= n
	}
	return parts, nil
}
