package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveDefaultPath(t *testing.T) {
	tests := []struct {
		name       string
		override   string
		systemOK   bool
		home       string
		homeErr    error
		want       string
		wantProbes int
		wantHomes  int
	}{
		{name: "override wins", override: "override", systemOK: true, home: "home", want: "override"},
		{name: "usable system path", systemOK: true, home: "home", want: "system", wantProbes: 1},
		{name: "home fallback", home: "home", want: filepath.Join("home", ".dockervc"), wantProbes: 1, wantHomes: 1},
		{name: "system fallback when home fails", homeErr: errors.New("no home"), want: "system", wantProbes: 1, wantHomes: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probes, homes := 0, 0
			got := resolveDefaultPath(tt.override, "system", func(path string) bool {
				probes++
				if path != "system" {
					t.Fatalf("probed %q, want system", path)
				}
				return tt.systemOK
			}, func() (string, error) {
				homes++
				return tt.home, tt.homeErr
			})
			if got != tt.want {
				t.Fatalf("resolveDefaultPath() = %q, want %q", got, tt.want)
			}
			if probes != tt.wantProbes || homes != tt.wantHomes {
				t.Fatalf("calls: probes=%d homes=%d, want probes=%d homes=%d", probes, homes, tt.wantProbes, tt.wantHomes)
			}
		})
	}
}
