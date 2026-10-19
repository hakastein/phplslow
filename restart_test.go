package main_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Whether a process built the index or reused it is seen through the diagnostic lines phplslow writes to stderr.
// They are not a protocol; these tests are their only reader.
const (
	built  = "phplslow: index built: "
	reused = "phplslow: index reused: "
)

func TestRestartReusesTheIndex(t *testing.T) {
	root, home := workspace(t, "methods"), cache(t)
	call := place{"src/Shop/Checkout.php", "add('apple')"}

	first := start(t, home)
	first.initialize(root)
	wantDefinition, wantReferences := definition(first, root, call), references(first, root, call, true)
	first.stop()

	second := start(t, home)
	second.initialize(root)
	gotDefinition, gotReferences := definition(second, root, call), references(second, root, call, true)
	second.stop()

	assert.Equal(t, wantDefinition, gotDefinition)
	assert.Equal(t, wantReferences, gotReferences)
	assert.NotEmpty(t, gotReferences)
	assert.Contains(t, first.stderr.String(), built)
	assert.Contains(t, second.stderr.String(), reused)
	assert.NotContains(t, second.stderr.String(), built)
}

func TestRestartAfterAChangeAnswersFromTheDisk(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, root string)
		want   []place
	}{
		{"file changed", appendToApp, append([]place{{"src/app.php", "add('fresh')"}}, cartAddCalls...)},
		{"file added", addShop, append([]place{{"src/shop.php", "add('added')"}}, cartAddCalls...)},
		{"file removed", removeCheckout, appCartAddCalls},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, home := workspace(t, "methods"), cache(t)
			first := start(t, home)
			first.initialize(root)
			references(first, root, cartAdd, false)
			first.stop()

			tt.change(t, root)
			second := start(t, home)
			second.initialize(root)
			got := references(second, root, cartAdd, false)
			second.stop()

			assert.ElementsMatch(t, locations(t, root, tt.want), got)
			assert.Contains(t, second.stderr.String(), built)
		})
	}
}

func appendToApp(t *testing.T, root string) {
	t.Helper()
	app := filepath.Join(root, "src", "app.php")
	src, err := os.ReadFile(app)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(app, append(src, "$basket->add('fresh');\n"...), 0o600))
}

func addShop(t *testing.T, root string) {
	t.Helper()
	src := "<?php\n\nfunction shop(): void\n{\n    $cart = new \\Shop\\Cart();\n    $cart->add('added');\n}\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "shop.php"), []byte(src), 0o600))
}

func removeCheckout(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(root, "src", "Shop", "Checkout.php")))
}
