package util

import (
	"sort"
	"testing"

	assert "github.com/stretchr/testify/require"
)

func TestStringSets(t *testing.T) {
	ret := NewStringSet([]string{"abc", "abc"}, []string{"efg", "abc"}).Keys()
	sort.Strings(ret)
	assert.Equal(t, []string{"abc", "efg"}, ret)

	assert.Empty(t, NewStringSet().Keys())
	assert.Equal(t, []string{"abc"}, NewStringSet([]string{"abc"}).Keys())
	assert.Equal(t, []string{"abc"}, NewStringSet([]string{"abc", "abc", "abc"}).Keys())
}

func TestStringSetKeys(t *testing.T) {
	expectedKeys := []string{"gamma", "beta", "alpha"}
	s := NewStringSet(append(expectedKeys, expectedKeys...))
	keys := s.Keys()
	assert.Equal(t, 3, len(keys))
	assert.True(t, In("alpha", keys))
	assert.True(t, In("beta", keys))
	assert.True(t, In("gamma", keys))

	s = nil
	keys = s.Keys()
	assert.Empty(t, keys)
}

func TestStringSetIntersect(t *testing.T) {
	someKeys := []string{"gamma", "beta", "alpha"}
	otherKeys := []string{"mu", "nu", "omicron"}
	a := NewStringSet(append(someKeys, otherKeys...))
	b := NewStringSet(someKeys)
	c := a.Intersect(b)

	keys := c.Keys()
	assert.Equal(t, 3, len(keys))
	assert.True(t, In("alpha", keys))
	assert.True(t, In("beta", keys))
	assert.True(t, In("gamma", keys))

	d := b.Intersect(a)
	keys = d.Keys()
	assert.Equal(t, 3, len(keys))
	assert.True(t, In("alpha", keys))
	assert.True(t, In("beta", keys))
	assert.True(t, In("gamma", keys))
}

func TestStringSetComplement(t *testing.T) {
	someKeys := []string{"gamma", "beta", "alpha"}
	otherKeys := []string{"mu", "nu", "omicron"}
	a := NewStringSet(append(someKeys, otherKeys...))
	b := NewStringSet(someKeys)
	c := a.Complement(b)

	keys := c.Keys()
	assert.Equal(t, 3, len(keys))
	assert.True(t, In("mu", keys))
	assert.True(t, In("nu", keys))
	assert.True(t, In("omicron", keys))

	d := b.Complement(a)
	assert.Empty(t, d.Keys())
}

func TestStringSetUnion(t *testing.T) {
	someKeys := []string{"gamma", "beta", "alpha", "zeta"}
	otherKeys := []string{"mu", "nu", "omicron", "zeta"}
	a := NewStringSet(otherKeys)
	b := NewStringSet(someKeys)
	c := a.Union(b)

	keys := c.Keys()
	assert.Equal(t, 7, len(keys))
	assert.True(t, In("alpha", keys))
	assert.True(t, In("beta", keys))
	assert.True(t, In("gamma", keys))
	assert.True(t, In("zeta", keys))
	assert.True(t, In("mu", keys))
	assert.True(t, In("nu", keys))
	assert.True(t, In("omicron", keys))

	d := b.Union(a)
	keys = d.Keys()
	assert.Equal(t, 7, len(keys))
	assert.True(t, In("alpha", keys))
	assert.True(t, In("beta", keys))
	assert.True(t, In("gamma", keys))
	assert.True(t, In("zeta", keys))
	assert.True(t, In("mu", keys))
	assert.True(t, In("nu", keys))
	assert.True(t, In("omicron", keys))
}
