package torrent

import (
	"fmt"
	"strings"
)

type BitField []byte

func (bf BitField) IsSet(index int) bool {
	byteIndex := index / 8
	offset := index % 8
	return (bf[byteIndex] & (1 << offset)) != 0
}

func (bf BitField) Set(index int) {
	byteIndex := index / 8
	offset := index % 8
	bf[byteIndex] |= (1 << offset)
}

func (bf BitField) String() string {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < len(bf); i++ {
		b.WriteString(fmt.Sprintf("%08b", bf[i]))
	}
	b.WriteString("]")
	return b.String()
}
