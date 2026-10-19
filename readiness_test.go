package main_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test holds the lock on the index file, so the index cannot be ready before the test releases it.
func TestRequestBeforeReadyWaitsForTheIndex(t *testing.T) {
	root, home := workspace(t, "methods"), cache(t)
	unlock := lockIndex(t, home, root)
	c := start(t, home)
	c.initialize(root)

	id := c.send("textDocument/references", referencesParams(t, root, place{"src/app.php", "add('kiwi')"}, false))
	require.True(t, c.silent(500*time.Millisecond), "answered before the index was ready")
	unlock()

	m := c.await(id)
	require.Nil(t, m.Error)
	assert.ElementsMatch(t, locations(t, root, cartAddCalls), decode(t, m.Result))
	c.stop()
}

func TestCancelEndsARequestThatWaitsForTheIndex(t *testing.T) {
	root, home := workspace(t, "methods"), cache(t)
	unlock := lockIndex(t, home, root)
	c := start(t, home)
	c.initialize(root)
	call := place{"src/app.php", "add('kiwi')"}

	waiting := c.send("textDocument/references", referencesParams(t, root, call, false))
	c.cancel(waiting)
	cancelled := c.await(waiting)
	unlock()
	finished := c.send("textDocument/definition", call.params(t, root))
	c.await(finished)
	c.cancel(finished)
	c.cancel(1000)

	require.NotNil(t, cancelled.Error)
	assert.Equal(t, -32800, cancelled.Error.Code)
	// The next response answers the next request: cancelling a finished or an unknown id sends nothing.
	assert.ElementsMatch(t, locations(t, root, cartAddCalls), references(c, root, call, false))
	c.stop()
}
