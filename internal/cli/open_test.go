package cli

import (
	"reflect"
	"testing"
)

func TestOpenCmdPerPlatform(t *testing.T) {
	cases := []struct {
		goos string
		want []string
	}{
		{"darwin", []string{"open", "/tmp/r.html"}},
		{"windows", []string{"rundll32", "url.dll,FileProtocolHandler", "/tmp/r.html"}},
		{"linux", []string{"xdg-open", "/tmp/r.html"}},
		{"freebsd", []string{"xdg-open", "/tmp/r.html"}},
	}
	for _, c := range cases {
		if got := openCmd(c.goos, "/tmp/r.html"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("openCmd(%q) = %v, want %v", c.goos, got, c.want)
		}
	}
}
