package main_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The syntax Workspace holds valid PHP that the parser must not lose: a class named Enum and its subclass, strings
// with the b prefix, and a real enum next to them.
var (
	enumLabel  = place{"src/Shop/Enum.php", "label(string $lang)"}
	enumCall   = place{"src/enums.php", "label('en')"}
	methodCall = place{"src/enums.php", "label('fr')"}
	syntaxAdd  = place{"src/Shop/Cart.php", "add(string $sku)"}
	bytesPack  = place{"src/bytes.php", "pack(string $y)"}
	packCall   = place{"src/bytes.php", "pack($y)"}
	suitColor  = place{"src/Shop/Suit.php", "color()"}

	bytesAddCalls = []place{
		{"src/bytes.php", "add(b'bin')"},
		{"src/bytes.php", `add(b"x{$y}")`},
	}
)

func TestSyntaxDefinition(t *testing.T) {
	root := workspace(t, "syntax")
	c := start(t, cache(t))
	c.initialize(root)

	tests := []struct {
		name string
		at   place
		want []place
	}{
		{"call on a class named Enum", enumCall, []place{enumLabel}},
		{"call on a subclass of Enum that does not declare the method", methodCall, nil},
		{"call with a b string argument", bytesAddCalls[0], []place{syntaxAdd}},
		{"call with an interpolated b string argument", bytesAddCalls[1], []place{syntaxAdd}},
		{"method of a class with b strings", packCall, []place{bytesPack}},
		{"method of a real enum, asked at its declaration", suitColor, []place{suitColor}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, locations(t, root, tt.want), definition(c, root, tt.at))
		})
	}
	c.stop()
}

func TestSyntaxReferences(t *testing.T) {
	root := workspace(t, "syntax")
	c := start(t, cache(t))
	c.initialize(root)

	tests := []struct {
		name string
		at   place
		want []place
	}{
		{"method of a class named Enum", enumLabel, []place{enumCall}},
		{"method called with b string arguments", syntaxAdd, bytesAddCalls},
		{"method of a class with b strings", bytesPack, []place{packCall}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, locations(t, root, tt.want), references(c, root, tt.at, false))
		})
	}
	c.stop()
}
