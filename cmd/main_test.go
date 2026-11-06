package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNamespaceConfig checks: namespace list parsing.
func TestNamespaceConfig(t *testing.T) {
	assert.Nil(t, namespaceConfig(""), "empty means every namespace")
	assert.Nil(t, namespaceConfig(" , "))
	got := namespaceConfig("a, b,,a")
	assert.Len(t, got, 2)
	assert.Contains(t, got, "a")
	assert.Contains(t, got, "b")
}
