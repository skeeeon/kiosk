package config

import "strings"

// EnsureServeBind makes PocketBase's `serve` subcommand honor the config's
// bind/port regardless of how the binary was invoked.
//
// The original main() logic only injected --http when len(os.Args)==1, which
// means `./kiosk-app serve` (e.g. from systemd or a Dockerfile ENTRYPOINT)
// silently fell back to PB's hardcoded 127.0.0.1:8090. This helper fixes
// that by inspecting the args.
//
// Rules:
//   - If args already include `--http` (with or without `=`), the user's
//     explicit flag wins; return unchanged.
//   - If args carry a non-serve subcommand (migrate, seed-catalog, etc.),
//     return unchanged — config bind/port doesn't apply to those flows.
//   - If args have no subcommand at all, default to `serve` and append the
//     `--http=<addr>` derived from config.
//   - If args explicitly say `serve` (with no --http), append `--http=<addr>`.
//
// addr is expected in "host:port" form (e.g. "0.0.0.0:8091"). Caller is
// responsible for formatting from cfg.Server.{Bind,Port}.
func EnsureServeBind(args []string, addr string) []string {
	for _, a := range args[1:] {
		if a == "--http" || strings.HasPrefix(a, "--http=") {
			return args
		}
	}
	sub := ""
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		sub = args[1]
	}
	switch sub {
	case "":
		return append(args, "serve", "--http="+addr)
	case "serve":
		return append(args, "--http="+addr)
	default:
		return args
	}
}
