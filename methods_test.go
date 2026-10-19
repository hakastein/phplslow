package main_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

var (
	cartAdd = place{"src/Shop/Cart.php", "add(string $sku): void"}
	wishAdd = place{"src/Shop/Wishlist.php", "add(string $sku): void"}

	checkoutCartAddCalls = []place{
		{"src/Shop/Checkout.php", "add('apple')"},
		{"src/Shop/Checkout.php", "ADD('pear')"},
	}
	appCartAddCalls = []place{
		{"src/app.php", "add('kiwi')"},
		{"src/app.php", "add('lime')"},
		{"src/app.php", "add('date')"},
		{"src/app.php", "add('emoji')"},
		{"src/app.php", "add('pea')"},
		{"src/app.php", "add('oat')"},
	}
	cartAddCalls = append(append([]place{}, checkoutCartAddCalls...), appCartAddCalls...)
)

func TestMethodDefinition(t *testing.T) {
	root := workspace(t, "methods")
	c := start(t, cache(t))
	c.initialize(root)

	tests := []struct {
		name string
		at   place
		want []place
	}{
		{"call on a variable assigned new", place{"src/Shop/Checkout.php", "add('apple')"}, []place{cartAdd}},
		{"method name in another case", place{"src/Shop/Checkout.php", "ADD('pear')"}, []place{cartAdd}},
		{"call on new in parentheses", place{"src/app.php", "add('lime')"}, []place{cartAdd}},
		{"nullsafe call", place{"src/app.php", "add('date')"}, []place{cartAdd}},
		{"call after Cyrillic and an emoji on the line", place{"src/app.php", "add('emoji')"}, []place{cartAdd}},
		{"variable of the arrow function's parent", place{"src/app.php", "add('pea')"}, []place{cartAdd}},
		{"variable imported by a closure", place{"src/app.php", "add('oat')"}, []place{cartAdd}},
		{"same method name of another class", place{"src/Shop/Checkout.php", "add('plum')"}, []place{wishAdd}},
		{"declaration itself", cartAdd, []place{cartAdd}},
		{"parameter of unknown class", place{"src/Shop/Checkout.php", "add('fig')"}, nil},
		{"variable assigned two classes", place{"src/app.php", "add('nut')"}, nil},
		{"call before the assignment", place{"src/app.php", "add('yam')"}, nil},
		{"method the class does not declare", place{"src/app.php", "remove('rye')"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, locations(t, root, tt.want), definition(c, root, tt.at))
		})
	}
	c.stop()
}

func TestMethodReferences(t *testing.T) {
	root := workspace(t, "methods")
	c := start(t, cache(t))
	c.initialize(root)

	tests := []struct {
		name        string
		at          place
		declaration bool
		want        []place
	}{
		{"from a call site", place{"src/app.php", "add('kiwi')"}, false, cartAddCalls},
		{"from the declaration", cartAdd, false, cartAddCalls},
		{"with the declaration", place{"src/Shop/Checkout.php", "add('apple')"}, true, append([]place{cartAdd}, cartAddCalls...)},
		{"same method name of another class", wishAdd, false, []place{{"src/Shop/Checkout.php", "add('plum')"}}},
		{"method with one call", place{"src/Shop/Cart.php", "total(): int"}, false, []place{{"src/Shop/Checkout.php", "total();"}}},
		{"unresolved call", place{"src/Shop/Checkout.php", "add('fig')"}, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, locations(t, root, tt.want), references(c, root, tt.at, tt.declaration))
		})
	}
	c.stop()
}

func TestRootWithASpaceAndAPercentSign(t *testing.T) {
	root := workspaceAt(t, "methods", "my ws 100%")
	c := start(t, cache(t))
	c.initialize(root)

	call := place{"src/app.php", "add('kiwi')"}
	assert.ElementsMatch(t, locations(t, root, []place{cartAdd}), definition(c, root, call))
	assert.ElementsMatch(t, locations(t, root, cartAddCalls), references(c, root, call, false))
	c.stop()
}
