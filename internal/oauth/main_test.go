package oauth

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	SetAllowPrivateHosts(true)
	os.Exit(m.Run())
}
