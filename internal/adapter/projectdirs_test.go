package adapter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProjectPresence_Newest(t *testing.T) {
	a := ProjectPresence{Path: "/p", LastActive: time.Unix(100, 0)}
	b := ProjectPresence{Path: "/p", LastActive: time.Unix(200, 0)}
	require.Equal(t, b.LastActive, NewerPresence(a, b).LastActive)
	require.Equal(t, b.LastActive, NewerPresence(b, a).LastActive)
}
