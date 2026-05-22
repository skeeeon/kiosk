package config

import (
	"reflect"
	"testing"
)

func TestEnsureServeBind(t *testing.T) {
	const addr = "0.0.0.0:8092"

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "no args -> default serve with --http",
			in:   []string{"kiosk-controller"},
			want: []string{"kiosk-controller", "serve", "--http=" + addr},
		},
		{
			name: "explicit serve with no --http gets one injected",
			in:   []string{"kiosk-controller", "serve"},
			want: []string{"kiosk-controller", "serve", "--http=" + addr},
		},
		{
			name: "user-provided --http= wins",
			in:   []string{"kiosk-controller", "serve", "--http=:9000"},
			want: []string{"kiosk-controller", "serve", "--http=:9000"},
		},
		{
			name: "user-provided --http separated wins",
			in:   []string{"kiosk-controller", "serve", "--http", ":9000"},
			want: []string{"kiosk-controller", "serve", "--http", ":9000"},
		},
		{
			name: "migrate subcommand is left alone",
			in:   []string{"kiosk-controller", "migrate"},
			want: []string{"kiosk-controller", "migrate"},
		},
		{
			name: "seed-catalog subcommand is left alone",
			in:   []string{"kiosk-controller", "seed-catalog", "--items=x.csv"},
			want: []string{"kiosk-controller", "seed-catalog", "--items=x.csv"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EnsureServeBind(tc.in, addr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}
