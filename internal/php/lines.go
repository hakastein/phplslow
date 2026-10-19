package php

import (
	"sort"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/VKCOM/php-parser/pkg/position"
)

// Position is an LSP position: zero-based line and UTF-16 code unit offset in the line.
type Position struct {
	Line      int
	Character int
}

// Range is an LSP range; End is exclusive.
type Range struct {
	Start Position
	End   Position
}

// lines turns byte offsets of a file into LSP positions.
type lines struct {
	src    []byte
	starts []int // byte offset of each line start
}

func newLines(src []byte) lines {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return lines{src: src, starts: starts}
}

func (l lines) span(p *position.Position) Range {
	return Range{Start: l.at(p.StartPos), End: l.at(p.EndPos)}
}

func (l lines) at(offset int) Position {
	offset = min(max(offset, 0), len(l.src))
	line := sort.Search(len(l.starts), func(i int) bool { return l.starts[i] > offset }) - 1
	units := 0
	for b := l.src[l.starts[line]:offset]; len(b) > 0; {
		r, size := utf8.DecodeRune(b)
		units += utf16.RuneLen(r) // an invalid byte decodes to U+FFFD, one unit
		b = b[size:]
	}
	return Position{Line: line, Character: units}
}
